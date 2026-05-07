package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/terrakube-community/terrakubed/internal/api/middleware"
	"github.com/terrakube-community/terrakubed/internal/api/repository"
)

// GraphQLHandler handles Elide-compatible GraphQL requests at /graphql/api/v1.
// Elide's GraphQL uses a connection/edges/node pattern:
//
//	query { organization { edges { node { id name } } } }
//
// This handler parses the query, extracts the resource type and requested fields,
// then maps to the generic repository.
type GraphQLHandler struct {
	repo *repository.GenericRepository
}

// NewGraphQLHandler creates a new handler.
func NewGraphQLHandler(repo *repository.GenericRepository) *GraphQLHandler {
	return &GraphQLHandler{repo: repo}
}

// GraphQL request/response types
type graphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   interface{} `json:"data,omitempty"`
	Errors []gqlError  `json:"errors,omitempty"`
}

type gqlError struct {
	Message string `json:"message"`
}

func (h *GraphQLHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeGQLError(w, "Failed to read request body")
		return
	}
	defer r.Body.Close()

	var req graphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeGQLError(w, "Invalid JSON")
		return
	}

	log.Printf("GraphQL query: %s", truncate(req.Query, 500))

	result, err := h.executeQuery(r, req.Query, req.Variables)
	if err != nil {
		log.Printf("GraphQL error: %v", err)
		writeGQLError(w, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	resp := graphQLResponse{Data: result}
	respBytes, _ := json.Marshal(resp)
	log.Printf("GraphQL response: %s", truncate(string(respBytes), 500))
	w.Write(respBytes)
}

// executeQuery parses an Elide-style GraphQL query and executes it.
func (h *GraphQLHandler) executeQuery(r *http.Request, query string, variables map[string]interface{}) (interface{}, error) {
	ctx := r.Context()
	query = strings.TrimSpace(query)

	// Determine if it's a query or mutation
	if strings.HasPrefix(query, "mutation") {
		return h.executeMutation(r, query, variables)
	}

	// Parse the root resource type from the query
	// Pattern: { resourceType { edges { node { field1 field2 } } } }
	// or: { resourceType(ids: ["..."]) { edges { node { field1 field2 } } } }
	rootType, ids, fields, relationships, pageParams, filterExpr := parseGraphQLQuery(query)
	if rootType == "" {
		return nil, fmt.Errorf("could not parse query")
	}

	log.Printf("GraphQL: type=%s ids=%v fields=%v rels=%v page=%+v filter=%q", rootType, ids, fields, relationships, pageParams, filterExpr)

	// Check if the resource type is registered
	meta, ok := h.repo.GetMeta(rootType)
	if !ok {
		return nil, fmt.Errorf("unknown resource type: %s", rootType)
	}

	// Resolve Elide filter expression to repository filter map using the meta
	filters := parseElideFilter(filterExpr, meta)

	// Build the result
	if len(ids) > 0 {
		// Fetch specific records by ID
		return h.fetchByIDs(ctx, rootType, meta, ids, fields, relationships)
	}

	// List all records (with optional pagination/filtering)
	return h.fetchAll(ctx, rootType, meta, fields, relationships, pageParams, filters)
}

func (h *GraphQLHandler) fetchByIDs(ctx context.Context, resourceType string, meta *repository.ResourceMeta, ids []string, fields []string, relationships []relInfo) (interface{}, error) {
	nodes := make([]map[string]interface{}, 0)

	for _, id := range ids {
		row, err := h.repo.FindByID(ctx, resourceType, id)
		if err != nil {
			continue
		}
		node := filterFields(row, fields)

		// Resolve relationships, passing the full row for FK access on parent rels
		for _, rel := range relationships {
			relData, err := h.resolveRelationship(ctx, id, row, meta, rel)
			if err != nil {
				log.Printf("GraphQL: error resolving rel %s for %s/%s: %v", rel.name, resourceType, id, err)
			} else {
				node[rel.name] = relData
			}
		}

		nodes = append(nodes, node)
	}

	return map[string]interface{}{
		resourceType: map[string]interface{}{
			"edges": wrapEdges(nodes),
		},
	}, nil
}

func (h *GraphQLHandler) fetchAll(ctx context.Context, resourceType string, meta *repository.ResourceMeta, fields []string, relationships []relInfo, page gqlPage, filters map[string]interface{}) (interface{}, error) {
	params := repository.ListParams{}
	if page.sort != "" {
		params.Sort = page.sort
	}
	if page.size > 0 {
		params.PageSize = page.size
		if page.number > 1 {
			params.PageOffset = (page.number - 1) * page.size
		}
	}
	if len(filters) > 0 {
		params.Filters = filters
	}
	rows, err := h.repo.List(ctx, resourceType, params)
	if err != nil {
		return nil, fmt.Errorf("failed to list %s: %w", resourceType, err)
	}

	nodes := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		node := filterFields(row, fields)

		// Resolve relationships, passing the full row for FK access on parent rels
		id := fmt.Sprintf("%v", row[meta.PKColumn])
		for _, rel := range relationships {
			relData, err := h.resolveRelationship(ctx, id, row, meta, rel)
			if err != nil {
				log.Printf("GraphQL: error resolving rel %s for %s/%s: %v", rel.name, resourceType, id, err)
			} else {
				node[rel.name] = relData
			}
		}

		nodes = append(nodes, node)
	}

	return map[string]interface{}{
		resourceType: map[string]interface{}{
			"edges": wrapEdges(nodes),
		},
	}, nil
}

// resolveRelationship resolves a single relationship for a parent resource node.
// parentRow is the full DB row for the parent (used to read FK values for to-one rels).
func (h *GraphQLHandler) resolveRelationship(ctx context.Context, parentID string, parentRow map[string]interface{}, parentMeta *repository.ResourceMeta, rel relInfo) (interface{}, error) {
	// ── To-one (parent) relationship ──
	if toOneRel, ok := parentMeta.Parents[rel.name]; ok {
		fkVal := parentRow[toOneRel.FKColumn]
		if fkVal == nil {
			return map[string]interface{}{"edges": wrapEdges([]map[string]interface{}{})}, nil
		}
		relMeta, ok := h.repo.GetMeta(toOneRel.ParentType)
		if !ok {
			return nil, fmt.Errorf("unknown type: %s", toOneRel.ParentType)
		}
		relRow, err := h.repo.FindByID(ctx, toOneRel.ParentType, fkVal)
		if err != nil || relRow == nil {
			return map[string]interface{}{"edges": wrapEdges([]map[string]interface{}{})}, nil
		}
		node := filterFields(relRow, rel.fields)
		relID := fmt.Sprintf("%v", relRow[relMeta.PKColumn])
		for _, subRel := range rel.rels {
			subData, err := h.resolveRelationship(ctx, relID, relRow, relMeta, subRel)
			if err == nil {
				node[subRel.name] = subData
			}
		}
		return map[string]interface{}{
			"edges": wrapEdges([]map[string]interface{}{node}),
		}, nil
	}

	// ── To-many (child) relationship ──
	childRel, ok := parentMeta.Children[rel.name]
	if !ok {
		return nil, fmt.Errorf("unknown relationship: %s", rel.name)
	}

	// Get child meta for PK column
	childMeta, _ := h.repo.GetMeta(childRel.ChildType)

	// Build the list of DB columns to SELECT:
	// - requested fields (camelCase → snake_case)
	// - PK column (needed for sub-relationships)
	// - FK column (needed for the WHERE clause)
	colSet := make(map[string]bool)
	colSet[childRel.FKColumn] = true
	if childMeta != nil {
		colSet[childMeta.PKColumn] = true
	}
	for _, f := range rel.fields {
		colSet[camelToSnake(f)] = true
	}
	selectCols := make([]string, 0, len(colSet))
	for c := range colSet {
		selectCols = append(selectCols, c)
	}

	// Query children using the FK column, selecting only needed columns
	params := repository.ListParams{
		ParentFK: childRel.FKColumn,
		ParentID: parentID,
		Columns:  selectCols,
		Sort:     rel.sort,
	}
	rows, err := h.repo.List(ctx, childRel.ChildType, params)
	if err != nil {
		return nil, err
	}

	nodes := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		node := filterFields(row, rel.fields)

		// Resolve nested sub-relationships (e.g., workspaceTag inside workspace)
		if childMeta != nil {
			childID := fmt.Sprintf("%v", row[childMeta.PKColumn])
			for _, subRel := range rel.rels {
				subData, err := h.resolveRelationship(ctx, childID, row, childMeta, subRel)
				if err == nil {
					node[subRel.name] = subData
				}
			}
		}

		nodes = append(nodes, node)
	}

	return map[string]interface{}{
		"edges": wrapEdges(nodes),
	}, nil
}

// executeMutation handles create/update/delete mutations.
func (h *GraphQLHandler) executeMutation(r *http.Request, query string, variables map[string]interface{}) (interface{}, error) {
	ctx := r.Context()
	// Elide mutations look like:
	// mutation { organization(op: UPSERT, data: {id: "...", name: "..."}) { edges { node { id name } } } }
	// mutation { organization(op: DELETE, ids: ["..."]) { edges { node { id } } } }
	log.Printf("GraphQL mutation: %s", truncate(query, 200))

	rootType, op, data := parseMutation(query, variables)
	if rootType == "" {
		return nil, fmt.Errorf("could not parse mutation")
	}

	_, ok := h.repo.GetMeta(rootType)
	if !ok {
		return nil, fmt.Errorf("unknown resource type: %s", rootType)
	}

	meta, _ := h.repo.GetMeta(rootType)

	switch strings.ToUpper(op) {
	case "UPSERT":
		id, _ := data["id"].(string)
		if id != "" {
			// Update — set updated_by / updated_date
			setGQLAuditUpdate(ctx, data, meta)
			if err := h.repo.Update(ctx, rootType, id, data); err != nil {
				return nil, err
			}
			row, err := h.repo.FindByID(ctx, rootType, id)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				rootType: map[string]interface{}{
					"edges": wrapEdges([]map[string]interface{}{row}),
				},
			}, nil
		}
		// Create — set created_by / updated_by / created_date / updated_date
		setGQLAuditCreate(ctx, data, meta)
		newID, err := h.repo.Create(ctx, rootType, data)
		if err != nil {
			return nil, err
		}
		row, err := h.repo.FindByID(ctx, rootType, newID)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			rootType: map[string]interface{}{
				"edges": wrapEdges([]map[string]interface{}{row}),
			},
		}, nil

	case "DELETE":
		id, _ := data["id"].(string)
		if id == "" {
			return nil, fmt.Errorf("DELETE requires id")
		}
		if err := h.repo.Delete(ctx, rootType, id); err != nil {
			return nil, err
		}
		return map[string]interface{}{rootType: nil}, nil

	default:
		return nil, fmt.Errorf("unknown operation: %s", op)
	}
}

// parseInlineData parses a GraphQL inline data body (content inside the outer braces)
// and populates the data map. Handles strings, booleans, numbers, and null.
//
// Supports:
//
//	key: "string value"
//	key: 'string value'
//	key: true / false
//	key: null
//	key: 123 / 1.5
func parseInlineData(body string, data map[string]interface{}) {
	body = strings.TrimSpace(body)
	i := 0
	for i < len(body) {
		// Skip whitespace and commas
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r' || body[i] == ',') {
			i++
		}
		if i >= len(body) {
			break
		}

		// Read key
		keyStart := i
		for i < len(body) && isWordChar(body[i]) {
			i++
		}
		if keyStart == i {
			i++ // skip unexpected char
			continue
		}
		key := body[keyStart:i]

		// Skip whitespace
		for i < len(body) && (body[i] == ' ' || body[i] == '\t') {
			i++
		}
		// Expect ':'
		if i >= len(body) || body[i] != ':' {
			continue
		}
		i++ // skip ':'

		// Skip whitespace
		for i < len(body) && (body[i] == ' ' || body[i] == '\t') {
			i++
		}
		if i >= len(body) {
			break
		}

		// Parse value
		switch body[i] {
		case '"', '\'':
			// String value
			quote := body[i]
			i++
			valStart := i
			for i < len(body) && body[i] != quote {
				if body[i] == '\\' {
					i++ // skip escaped char
				}
				i++
			}
			data[key] = body[valStart:i]
			if i < len(body) {
				i++ // skip closing quote
			}

		case '{':
			// Nested object — skip for now (advance past the block)
			depth := 0
			for i < len(body) {
				if body[i] == '{' {
					depth++
				} else if body[i] == '}' {
					depth--
					if depth == 0 {
						i++
						break
					}
				}
				i++
			}

		default:
			// Boolean, number, or null
			valStart := i
			for i < len(body) && body[i] != ',' && body[i] != '\n' && body[i] != '}' {
				i++
			}
			raw := strings.TrimSpace(body[valStart:i])
			switch raw {
			case "true":
				data[key] = true
			case "false":
				data[key] = false
			case "null":
				data[key] = nil
			default:
				// Try integer then float
				if iv, err := strconv.ParseInt(raw, 10, 64); err == nil {
					data[key] = iv
				} else if fv, err := strconv.ParseFloat(raw, 64); err == nil {
					data[key] = fv
				} else {
					data[key] = raw // fallback: keep as string
				}
			}
		}
	}
}

// ──────────────────────────────────────────────────
// Query parsing helpers — proper brace-matching
// ──────────────────────────────────────────────────

type relInfo struct {
	name   string
	sort   string    // e.g. "name" or "-name" (from sort: "name" arg)
	fields []string
	rels   []relInfo // nested relationships
}

// gqlPage holds optional pagination params parsed from a GraphQL query.
type gqlPage struct {
	size   int // 0 = no limit
	number int // 1-based
	sort   string
}

var (
	// Matches root type: { organization { ... } }
	rootTypeRe = regexp.MustCompile(`(?:query\s*(?:\w+)?\s*)?[{]\s*(\w+)`)
	// Matches ids parameter: ids: ["id1", "id2"] (anywhere in args)
	idsRe = regexp.MustCompile(`\bids?\s*:\s*\[([^\]]*)\]`)
	// Matches single id filter: id: "uuid" (anywhere in args)
	singleIDRe = regexp.MustCompile(`\bids?\s*:\s*"([^"]+)"`)
	// Matches pagination: (pagination: {number: N, size: M})
	pageRe = regexp.MustCompile(`pagination\s*:\s*\{[^}]*number\s*:\s*(\d+)[^}]*size\s*:\s*(\d+)[^}]*\}`)
	pageRe2 = regexp.MustCompile(`pagination\s*:\s*\{[^}]*size\s*:\s*(\d+)[^}]*number\s*:\s*(\d+)[^}]*\}`)
	// Matches first/offset: (first: N, offset: M)
	firstRe  = regexp.MustCompile(`first\s*:\s*(\d+)`)
	offsetRe = regexp.MustCompile(`offset\s*:\s*(\d+)`)
)

func parseGraphQLQuery(query string) (rootType string, ids []string, fields []string, relationships []relInfo, page gqlPage, filterExpr string) {
	// Extract root type
	matches := rootTypeRe.FindStringSubmatch(query)
	if len(matches) < 2 {
		return
	}
	rootType = matches[1]

	// Find content after root type name
	inner := query
	if idx := strings.Index(inner, rootType); idx >= 0 {
		inner = inner[idx+len(rootType):]
	}

	// Extract IDs if present
	if idMatches := idsRe.FindStringSubmatch(inner); len(idMatches) >= 2 {
		for _, id := range strings.Split(idMatches[1], ",") {
			id = strings.Trim(strings.TrimSpace(id), `"'`)
			if id != "" {
				ids = append(ids, id)
			}
		}
	} else if singleMatch := singleIDRe.FindStringSubmatch(inner); len(singleMatch) >= 2 {
		ids = append(ids, singleMatch[1])
	}

	// Extract root args (between the first pair of parens after root type)
	var rootArgs string
	if idx := strings.Index(inner, "("); idx >= 0 {
		rootArgs = extractParenContent(inner, idx)
	}

	if rootArgs != "" {
		// sort
		page.sort = parseSortArg(rootArgs)

		// Elide filter: filter: "expression"
		filterExpr = parseFilterArg(rootArgs)

		// Elide pagination: (pagination: {number: 1, size: 20})
		if m := pageRe.FindStringSubmatch(rootArgs); len(m) >= 3 {
			page.number, _ = strconv.Atoi(m[1])
			page.size, _ = strconv.Atoi(m[2])
		} else if m := pageRe2.FindStringSubmatch(rootArgs); len(m) >= 3 {
			page.size, _ = strconv.Atoi(m[1])
			page.number, _ = strconv.Atoi(m[2])
		} else {
			// first/offset style
			if m := firstRe.FindStringSubmatch(rootArgs); len(m) >= 2 {
				page.size, _ = strconv.Atoi(m[1])
			}
			if m := offsetRe.FindStringSubmatch(rootArgs); len(m) >= 2 {
				offset, _ := strconv.Atoi(m[1])
				if page.size > 0 {
					page.number = offset/page.size + 1
				}
			}
		}
	}

	// Find the root node's body using brace matching
	// We need to find: edges { node { BODY } }
	nodeBody := findNodeBody(inner)
	if nodeBody == "" {
		return
	}

	// Parse the node body into fields and relationships
	fields, relationships = parseNodeBody(nodeBody)
	return
}

// parseFilterArg extracts the value of a filter: "..." argument from GraphQL args.
func parseFilterArg(args string) string {
	filterRe := regexp.MustCompile(`filter\s*:\s*"([^"]*)"`)
	m := filterRe.FindStringSubmatch(args)
	if len(m) >= 2 {
		return m[1]
	}
	// Also match single-quoted filter
	filterRe2 := regexp.MustCompile(`filter\s*:\s*'([^']*)'`)
	m = filterRe2.FindStringSubmatch(args)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

// parseElideFilter parses an Elide RSQL filter expression into a repository filter map.
// Supported patterns:
//   - "field=='value'"           → Filters["field"] = "value"
//   - "parentType.id=='uuid'"    → Filters[fk_column] = "uuid"  (resolved via meta.Parents)
//   - Multiple with ';' or ','   → AND-combined (all conditions applied)
func parseElideFilter(expr string, meta *repository.ResourceMeta) map[string]interface{} {
	if expr == "" {
		return nil
	}

	// Split on ';' or ',' (Elide uses ';' for AND, ',' for OR — we only support AND)
	conditions := regexp.MustCompile(`[;,]`).Split(expr, -1)
	filters := make(map[string]interface{})

	// Pattern: field==['value'] or field=='value'
	eqRe := regexp.MustCompile(`^([a-zA-Z0-9_.]+)==\[?'?([^'\]]*)'?\]?$`)

	for _, cond := range conditions {
		cond = strings.TrimSpace(cond)
		m := eqRe.FindStringSubmatch(cond)
		if len(m) < 3 {
			continue
		}
		field, value := m[1], m[2]

		// Handle "parentType.attribute" → look up FK column in Parents
		if dot := strings.Index(field, "."); dot >= 0 {
			parentTypeName := field[:dot]
			// attribute := field[dot+1:] // e.g. "id" — we use the FK column regardless

			if meta != nil {
				// Find the FK column for this parent type
				for _, parent := range meta.Parents {
					if parent.ParentType == parentTypeName {
						filters[parent.FKColumn] = value
						break
					}
				}
			} else {
				// No meta — fall back to "parentType_id" convention
				filters[camelToSnake(parentTypeName)+"_id"] = value
			}
			continue
		}

		// Simple field equality — convert camelCase to snake_case
		col := camelToSnake(field)
		filters[col] = value
	}

	if len(filters) == 0 {
		return nil
	}
	return filters
}

// findNodeBody finds the content inside the first `node { ... }` with proper brace matching.
func findNodeBody(s string) string {
	// Find "node" keyword followed by "{"
	idx := 0
	for {
		pos := strings.Index(s[idx:], "node")
		if pos < 0 {
			return ""
		}
		pos += idx
		// Make sure it's the word "node" (not part of another word)
		after := pos + 4
		if after < len(s) && (s[after] == ' ' || s[after] == '\t' || s[after] == '\n' || s[after] == '{') {
			// Find the opening brace
			braceStart := strings.Index(s[after:], "{")
			if braceStart < 0 {
				return ""
			}
			braceStart += after
			return extractBraceContent(s, braceStart)
		}
		idx = pos + 4
	}
}

// extractParenContent extracts content between matching parens starting at position of '('.
func extractParenContent(s string, openPos int) string {
	depth := 0
	for i := openPos; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[openPos+1 : i]
			}
		}
	}
	return ""
}

// parseSortArg extracts the value of a sort: "..." argument.
// Returns empty string if not present.
func parseSortArg(args string) string {
	sortRe := regexp.MustCompile(`sort\s*:\s*"([^"]+)"`)
	m := sortRe.FindStringSubmatch(args)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

// extractBraceContent extracts content between matching braces starting at position of '{'.
func extractBraceContent(s string, openPos int) string {
	depth := 0
	for i := openPos; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[openPos+1 : i]
			}
		}
	}
	return ""
}

// parseNodeBody parses the content inside a node { ... } block into scalar fields and relationships.
// A relationship is identified by a word followed by optional args and a `{`.
func parseNodeBody(body string) (fields []string, rels []relInfo) {
	body = strings.TrimSpace(body)
	i := 0

	for i < len(body) {
		// Skip whitespace
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
			i++
		}
		if i >= len(body) {
			break
		}

		// Read a word (field or relationship name)
		wordStart := i
		for i < len(body) && isWordChar(body[i]) {
			i++
		}
		if wordStart == i {
			i++ // skip non-word char
			continue
		}
		word := body[wordStart:i]

		// Skip whitespace
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
			i++
		}

		// Parse optional arguments: (sort: "name", filter: ...)
		// We capture sort so child relationships can be ordered correctly.
		var relSort string
		if i < len(body) && body[i] == '(' {
			// Extract the args content
			argsContent := extractParenContent(body, i)
			relSort = parseSortArg(argsContent)
			// Advance past the closing paren
			depth := 0
			for i < len(body) {
				if body[i] == '(' {
					depth++
				} else if body[i] == ')' {
					depth--
					if depth == 0 {
						i++
						break
					}
				}
				i++
			}
			// Skip whitespace after args
			for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
				i++
			}
		}

		// Check if this is a relationship (has a { block) or a scalar field
		if i < len(body) && body[i] == '{' {
			// It's a relationship — extract its brace content
			content := extractBraceContent(body, i)
			// Advance past the closing brace
			depth := 0
			for i < len(body) {
				if body[i] == '{' {
					depth++
				} else if body[i] == '}' {
					depth--
					if depth == 0 {
						i++
						break
					}
				}
				i++
			}

			// Parse the relationship's inner content
			// Look for edges { node { FIELDS } } pattern inside
			relNodeBody := findNodeBody(content)
			if relNodeBody != "" {
				relFields, subRels := parseNodeBody(relNodeBody)
				rels = append(rels, relInfo{name: word, sort: relSort, fields: relFields, rels: subRels})
			}
		} else {
			// It's a scalar field
			fields = append(fields, word)
		}
	}
	return
}

func isWordChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

func parseMutation(query string, variables map[string]interface{}) (rootType string, op string, data map[string]interface{}) {
	data = make(map[string]interface{})

	// Extract root type after "mutation"
	matches := regexp.MustCompile(`mutation\s*(?:\w+)?\s*[{]\s*(\w+)`).FindStringSubmatch(query)
	if len(matches) < 2 {
		return
	}
	rootType = matches[1]

	// Extract operation
	opMatch := regexp.MustCompile(`op\s*:\s*(\w+)`).FindStringSubmatch(query)
	if len(opMatch) >= 2 {
		op = opMatch[1]
	}

	// Extract data from variables if present — variables always take precedence over inline data.
	gotFromVariables := false
	if variables != nil {
		if d, ok := variables["data"]; ok {
			if dm, ok := d.(map[string]interface{}); ok {
				data = dm
				gotFromVariables = true
			}
		}
	}

	// Only parse inline data block when variables didn't provide it.
	// This prevents inline data from silently overwriting variable values.
	if !gotFromVariables {
		if dataIdx := strings.Index(query, "data:"); dataIdx >= 0 {
			afterData := strings.TrimSpace(query[dataIdx+5:])
			if braceIdx := strings.Index(afterData, "{"); braceIdx >= 0 {
				dataBody := extractBraceContent(afterData, braceIdx)
				if dataBody != "" {
					parseInlineData(dataBody, data)
				}
			}
		}
	}

	// Extract IDs for delete
	if idsMatch := idsRe.FindStringSubmatch(query); len(idsMatch) >= 2 {
		for _, id := range strings.Split(idsMatch[1], ",") {
			id = strings.Trim(strings.TrimSpace(id), `"'`)
			if id != "" {
				data["id"] = id
				break
			}
		}
	}

	return
}

// ──────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────

// filterFields selects requested fields from a row and converts keys to camelCase.
// GraphQL queries use camelCase (terraformVersion) but DB returns snake_case (terraform_version).
func filterFields(row map[string]interface{}, fields []string) map[string]interface{} {
	if len(fields) == 0 {
		// Return all fields, converted to camelCase
		result := make(map[string]interface{}, len(row))
		for k, v := range row {
			result[snakeToCamel(k)] = v
		}
		return result
	}

	result := make(map[string]interface{})
	for _, f := range fields {
		// Try exact match first (camelCase from DB already converted)
		if v, ok := row[f]; ok {
			result[f] = v
			continue
		}
		// Try snake_case version (UI sends camelCase, DB has snake_case)
		snakeF := camelToSnake(f)
		if v, ok := row[snakeF]; ok {
			result[f] = v
		}
	}
	return result
}

// snakeToCamel converts snake_case to camelCase: "terraform_version" → "terraformVersion"
func snakeToCamel(s string) string {
	parts := strings.Split(s, "_")
	if len(parts) <= 1 {
		return s
	}
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) > 0 {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// camelToSnake converts camelCase to snake_case: "terraformVersion" → "terraform_version"
func camelToSnake(s string) string {
	var result strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				result.WriteByte('_')
			}
			result.WriteRune(r + 32) // toLower
		} else {
			result.WriteRune(r)
		}
	}
	return result.String()
}

func wrapEdges(nodes []map[string]interface{}) []map[string]interface{} {
	edges := make([]map[string]interface{}, len(nodes))
	for i, node := range nodes {
		edges[i] = map[string]interface{}{"node": node}
	}
	return edges
}

func writeGQLError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // GraphQL always returns 200
	json.NewEncoder(w).Encode(graphQLResponse{
		Errors: []gqlError{{Message: msg}},
	})
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// setGQLAuditCreate sets audit columns for GraphQL UPSERT (create path).
func setGQLAuditCreate(ctx context.Context, data map[string]interface{}, meta *repository.ResourceMeta) {
	now := time.Now()
	email := gqlUserEmail(ctx)
	setIfColExists(data, meta, "created_by", email)
	setIfColExists(data, meta, "updated_by", email)
	setIfColExists(data, meta, "created_date", now)
	setIfColExists(data, meta, "updated_date", now)
}

// setGQLAuditUpdate sets audit columns for GraphQL UPSERT (update path).
func setGQLAuditUpdate(ctx context.Context, data map[string]interface{}, meta *repository.ResourceMeta) {
	now := time.Now()
	email := gqlUserEmail(ctx)
	setIfColExists(data, meta, "updated_by", email)
	setIfColExists(data, meta, "updated_date", now)
}

func setIfColExists(data map[string]interface{}, meta *repository.ResourceMeta, col string, val interface{}) {
	if meta == nil {
		return
	}
	for _, c := range meta.Columns {
		if c == col {
			data[col] = val
			return
		}
	}
}

func gqlUserEmail(ctx context.Context) string {
	if user := middleware.GetUser(ctx); user != nil {
		return user.Email
	}
	return ""
}
