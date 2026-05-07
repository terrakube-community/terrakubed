package handler

import (
	"testing"
)

// ──────────────────────────────────────────────────
// parseInlineData tests
// ──────────────────────────────────────────────────

func TestParseInlineData_Strings(t *testing.T) {
	data := make(map[string]interface{})
	parseInlineData(`name: "my-workspace", description: "hello world"`, data)

	if data["name"] != "my-workspace" {
		t.Errorf("name = %v, want %q", data["name"], "my-workspace")
	}
	if data["description"] != "hello world" {
		t.Errorf("description = %v, want %q", data["description"], "hello world")
	}
}

func TestParseInlineData_Booleans(t *testing.T) {
	data := make(map[string]interface{})
	parseInlineData(`locked: true, allowRemoteApply: false`, data)

	if v, ok := data["locked"].(bool); !ok || !v {
		t.Errorf("locked = %v (%T), want true (bool)", data["locked"], data["locked"])
	}
	if v, ok := data["allowRemoteApply"].(bool); !ok || v {
		t.Errorf("allowRemoteApply = %v (%T), want false (bool)", data["allowRemoteApply"], data["allowRemoteApply"])
	}
}

func TestParseInlineData_Integers(t *testing.T) {
	data := make(map[string]interface{})
	parseInlineData(`priority: 100`, data)

	if v, ok := data["priority"].(int64); !ok || v != 100 {
		t.Errorf("priority = %v (%T), want 100 (int64)", data["priority"], data["priority"])
	}
}

func TestParseInlineData_Null(t *testing.T) {
	data := make(map[string]interface{})
	parseInlineData(`vcsId: null, name: "test"`, data)

	if _, exists := data["vcsId"]; !exists {
		t.Error("vcsId key missing")
	}
	if data["vcsId"] != nil {
		t.Errorf("vcsId = %v, want nil", data["vcsId"])
	}
	if data["name"] != "test" {
		t.Errorf("name = %v, want test", data["name"])
	}
}

func TestParseInlineData_MixedTypes(t *testing.T) {
	data := make(map[string]interface{})
	parseInlineData(`id: "abc-123", locked: true, priority: 5, description: ""`, data)

	if data["id"] != "abc-123" {
		t.Errorf("id = %v, want abc-123", data["id"])
	}
	if v, ok := data["locked"].(bool); !ok || !v {
		t.Errorf("locked = %v (%T), want true (bool)", data["locked"], data["locked"])
	}
}

// ──────────────────────────────────────────────────
// parseGraphQLQuery tests
// ──────────────────────────────────────────────────

func TestParseGraphQLQuery_BasicQuery(t *testing.T) {
	query := `{ organization { edges { node { id name } } } }`
	rootType, ids, fields, _, _, _ := parseGraphQLQuery(query)

	if rootType != "organization" {
		t.Errorf("rootType = %q, want organization", rootType)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty", ids)
	}
	if len(fields) == 0 {
		t.Error("fields should not be empty")
	}
}

func TestParseGraphQLQuery_WithIDs(t *testing.T) {
	query := `{ workspace(ids: ["uuid-123"]) { edges { node { id name } } } }`
	rootType, ids, _, _, _, _ := parseGraphQLQuery(query)

	if rootType != "workspace" {
		t.Errorf("rootType = %q, want workspace", rootType)
	}
	if len(ids) != 1 || ids[0] != "uuid-123" {
		t.Errorf("ids = %v, want [uuid-123]", ids)
	}
}

func TestParseGraphQLQuery_WithFilter(t *testing.T) {
	query := `{ workspace(filter: "organization.id=='org-uuid'") { edges { node { id name } } } }`
	_, _, _, _, _, filterExpr := parseGraphQLQuery(query)

	if filterExpr != "organization.id=='org-uuid'" {
		t.Errorf("filterExpr = %q, want organization.id=='org-uuid'", filterExpr)
	}
}

func TestParseGraphQLQuery_WithPagination(t *testing.T) {
	query := `{ workspace(pagination: {number: 2, size: 10}) { edges { node { id } } } }`
	_, _, _, _, page, _ := parseGraphQLQuery(query)

	if page.number != 2 {
		t.Errorf("page.number = %d, want 2", page.number)
	}
	if page.size != 10 {
		t.Errorf("page.size = %d, want 10", page.size)
	}
}

func TestParseGraphQLQuery_WithSort(t *testing.T) {
	query := `{ workspace(sort: "-name") { edges { node { id name } } } }`
	_, _, _, _, page, _ := parseGraphQLQuery(query)

	if page.sort != "-name" {
		t.Errorf("page.sort = %q, want -name", page.sort)
	}
}

// ──────────────────────────────────────────────────
// parseElideFilter tests
// ──────────────────────────────────────────────────

func TestParseElideFilter_SimpleEquality(t *testing.T) {
	filters := parseElideFilter("name=='test'", nil)
	if len(filters) == 0 {
		t.Fatal("expected non-empty filters")
	}
	if filters["name"] != "test" {
		t.Errorf("name = %v, want test", filters["name"])
	}
}

func TestParseElideFilter_Empty(t *testing.T) {
	filters := parseElideFilter("", nil)
	if filters != nil {
		t.Errorf("expected nil for empty filter, got %v", filters)
	}
}

func TestParseElideFilter_CamelCaseFieldConversion(t *testing.T) {
	filters := parseElideFilter("workspaceId=='uuid-abc'", nil)
	if filters["workspace_id"] != "uuid-abc" {
		t.Errorf("workspace_id = %v, want uuid-abc", filters["workspace_id"])
	}
}

// ──────────────────────────────────────────────────
// parseMutation tests
// ──────────────────────────────────────────────────

func TestParseMutation_BasicUpsert(t *testing.T) {
	query := `mutation { workspace(op: UPSERT, data: { name: "test-ws", locked: false }) { edges { node { id } } } }`
	rootType, op, data := parseMutation(query, nil)

	if rootType != "workspace" {
		t.Errorf("rootType = %q, want workspace", rootType)
	}
	if op != "UPSERT" {
		t.Errorf("op = %q, want UPSERT", op)
	}
	if data["name"] != "test-ws" {
		t.Errorf("name = %v, want test-ws", data["name"])
	}
	if v, ok := data["locked"].(bool); !ok || v {
		t.Errorf("locked = %v (%T), want false (bool)", data["locked"], data["locked"])
	}
}

func TestParseMutation_DeleteWithIDs(t *testing.T) {
	query := `mutation { workspace(op: DELETE, ids: ["uuid-456"]) { edges { node { id } } } }`
	rootType, op, data := parseMutation(query, nil)

	if rootType != "workspace" {
		t.Errorf("rootType = %q, want workspace", rootType)
	}
	if op != "DELETE" {
		t.Errorf("op = %q, want DELETE", op)
	}
	if data["id"] != "uuid-456" {
		t.Errorf("id = %v, want uuid-456", data["id"])
	}
}

func TestParseMutation_VariablesTakePrecedence(t *testing.T) {
	// Variables should override inline data
	query := `mutation { workspace(op: UPSERT, data: { name: "inline-name" }) { edges { node { id } } } }`
	variables := map[string]interface{}{
		"data": map[string]interface{}{
			"name": "var-name",
		},
	}
	_, _, data := parseMutation(query, variables)

	// Variables should take precedence when provided
	if data["name"] != "var-name" {
		t.Errorf("name = %v, want var-name (from variables)", data["name"])
	}
}

// ──────────────────────────────────────────────────
// camelToSnake / snakeToCamel tests
// ──────────────────────────────────────────────────

func TestCamelToSnake(t *testing.T) {
	cases := []struct{ in, want string }{
		{"terraformVersion", "terraform_version"},
		{"organizationId", "organization_id"},
		{"name", "name"},
		{"ID", "i_d"},
		{"vcsId", "vcs_id"},
	}
	for _, c := range cases {
		got := camelToSnake(c.in)
		if got != c.want {
			t.Errorf("camelToSnake(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSnakeToCamel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"terraform_version", "terraformVersion"},
		{"organization_id", "organizationId"},
		{"name", "name"},
		{"vcs_id", "vcsId"},
	}
	for _, c := range cases {
		got := snakeToCamel(c.in)
		if got != c.want {
			t.Errorf("snakeToCamel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
