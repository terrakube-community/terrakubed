import { stripAnsi } from "./stripAnsi";
import { PlanChange, getPlanChangeActionLabel } from "./structuredPlan";

export type ApplyResourceState = "pending" | "running" | "done" | "error";

export type ApplyResource = {
  address: string;
  action: "create" | "update" | "delete" | "replace" | "read";
  state: ApplyResourceState;
  detail?: string;
  /** This resource's own console lines (progress and, on failure, the error block). */
  log: string[];
};

export type ApplyProgress = {
  resources: ApplyResource[];
  errors: string[];
};

const ACTION_BY_VERB: Record<string, ApplyResource["action"]> = {
  Creating: "create",
  Creation: "create",
  Modifying: "update",
  Modifications: "update",
  Destroying: "delete",
  Destruction: "delete",
  Reading: "read",
  Read: "read",
};

const STARTED = /^(.+?): (Creating|Modifying|Destroying|Reading)\.\.\.$/;
const STILL = /^(.+?): Still (creating|modifying|destroying|reading)\.\.\. \[(.+) elapsed\]$/;
const FINISHED = /^(.+?): (Creation|Modifications|Destruction|Read) complete after (\S+)/;
// Error blocks look like "Error: msg" followed by "with <address>," inside a box-drawn frame.
const ERROR_START = /^[│\s]*Error: (.*)$/;
const ERROR_WITH = /^[│\s]*with (.+?),\s*$/;

// Data source reads are refresh noise, not changes: Terraform Cloud leaves them out of apply too.
const isDataSource = (address: string) => /(^|\.)data\./.test(address);

export type PlannedResource = { address: string; action: ApplyResource["action"] };

/** Turns the plan step's structured changes into the list of resources an apply will touch. */
export const plannedFromChanges = (changes?: PlanChange[]): PlannedResource[] => {
  const planned: PlannedResource[] = [];
  for (const change of changes ?? []) {
    if (!change.address || isDataSource(change.address)) {
      continue;
    }
    const action = getPlanChangeActionLabel(change.actions ?? [], change.action);
    if (action === "create" || action === "update" || action === "delete" || action === "replace") {
      planned.push({ address: change.address, action });
    }
  }
  return planned;
};

/**
 * Parses terraform apply/destroy console output into per-resource progress.
 * Resources start "running" (yellow), turn "done" (green) on a "complete" line
 * and "error" (red) when an Error block names them with a "with <address>" line.
 */
export const parseApplyProgress = (log?: string, planned: PlannedResource[] = []): ApplyProgress => {
  const resources = new Map<string, ApplyResource>();
  const errors: string[] = [];
  const halves = new Map<string, number>();
  // Seed from the plan so not-yet-started resources show as pending, in plan order.
  for (const p of planned) {
    resources.set(p.address, { address: p.address, action: p.action, state: "pending", log: [] });
  }
  if (!log) {
    return { resources: Array.from(resources.values()), errors };
  }

  const lines = stripAnsi(log).split(/\r?\n/);
  let pendingError: string | undefined;
  // Lines of the Error block being read (they are boxed with │ and end at ╵).
  let errorBlock: string[] | undefined;

  const closeErrorBlock = () => {
    if (!errorBlock) {
      return;
    }
    const withLine = errorBlock.map((l) => ERROR_WITH.exec(l)).find((m) => m !== null);
    if (withLine && pendingError !== undefined) {
      const address = withLine[1];
      const current = resources.get(address) ?? {
        address,
        action: "create" as const,
        state: "running" as const,
        log: [],
      };
      resources.set(address, {
        ...current,
        state: "error",
        detail: pendingError,
        log: [...current.log, ...errorBlock],
      });
    }
    errorBlock = undefined;
    pendingError = undefined;
  };

  for (const raw of lines) {
    const line = raw.trim();

    if (errorBlock) {
      if (line.startsWith("│")) {
        errorBlock.push(line.replace(/^│\s?/, ""));
        continue;
      }
      closeErrorBlock();
    }

    const started = STARTED.exec(line);
    if (started) {
      const [, address, verb] = started;
      if (isDataSource(address)) {
        continue;
      }
      const previous = resources.get(address);
      const action = ACTION_BY_VERB[verb];
      // A replace shows up as Destroying then Creating on the same address (either order).
      const merged =
        previous?.action === "replace" || (previous && previous.state !== "pending" && previous.action !== action)
          ? "replace"
          : action;
      resources.set(address, { address, action: merged, state: "running", log: [...(previous?.log ?? []), line] });
      continue;
    }

    const still = STILL.exec(line);
    if (still) {
      const current = resources.get(still[1]);
      if (current) {
        current.detail = `${still[3]} elapsed`;
        current.log.push(line);
      }
      continue;
    }

    const finished = FINISHED.exec(line);
    if (finished) {
      const [, address, verb, duration] = finished;
      if (isDataSource(address)) {
        continue;
      }
      const current = resources.get(address) ?? {
        address,
        action: ACTION_BY_VERB[verb],
        state: "running" as const,
        log: [],
      };
      // The first half of a replace finishes (Destruction) while the resource is still being recreated.
      // A replace is two operations (destroy + create, in either order): only the second completion finishes it.
      const completions = (halves.get(address) ?? 0) + 1;
      halves.set(address, completions);
      const midReplace = current.action === "replace" && completions < 2;
      resources.set(address, {
        ...current,
        state: midReplace ? "running" : "done",
        detail: midReplace ? current.detail : `after ${duration}`,
        log: [...current.log, line],
      });
      continue;
    }

    const errStart = ERROR_START.exec(line);
    if (errStart) {
      pendingError = errStart[1].trim();
      errors.push(pendingError);
      errorBlock = [`Error: ${pendingError}`];
    }
  }
  closeErrorBlock();

  return { resources: Array.from(resources.values()), errors };
};

export const hasApplyProgress = (log?: string, planned: PlannedResource[] = []): boolean =>
  planned.length > 0 || parseApplyProgress(log).resources.length > 0;
