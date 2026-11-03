import { stripAnsi } from "./stripAnsi";

export type ApplyResourceState = "running" | "done" | "error";

export type ApplyResource = {
  address: string;
  action: "create" | "update" | "delete" | "replace" | "read";
  state: ApplyResourceState;
  detail?: string;
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

/**
 * Parses terraform apply/destroy console output into per-resource progress.
 * Resources start "running" (yellow), turn "done" (green) on a "complete" line
 * and "error" (red) when an Error block names them with a "with <address>" line.
 */
export const parseApplyProgress = (log?: string): ApplyProgress => {
  const resources = new Map<string, ApplyResource>();
  const errors: string[] = [];
  if (!log) {
    return { resources: [], errors };
  }

  const lines = stripAnsi(log).split(/\r?\n/);
  let pendingError: string | undefined;

  for (const raw of lines) {
    const line = raw.trim();

    const started = STARTED.exec(line);
    if (started) {
      const [, address, verb] = started;
      const previous = resources.get(address);
      const action = ACTION_BY_VERB[verb];
      // A replace shows up as Destroying then Creating on the same address.
      const merged = previous && previous.action === "delete" && action === "create" ? "replace" : action;
      resources.set(address, { address, action: merged, state: "running" });
      continue;
    }

    const still = STILL.exec(line);
    if (still) {
      const current = resources.get(still[1]);
      if (current) {
        current.detail = `${still[3]} elapsed`;
      }
      continue;
    }

    const finished = FINISHED.exec(line);
    if (finished) {
      const [, address, verb, duration] = finished;
      const current = resources.get(address) ?? { address, action: ACTION_BY_VERB[verb], state: "running" as const };
      // The first half of a replace finishes (Destruction) while the resource is still being recreated.
      const midReplace = current.action === "replace" && verb === "Destruction";
      resources.set(address, {
        ...current,
        state: midReplace ? "running" : "done",
        detail: midReplace ? current.detail : `after ${duration}`,
      });
      continue;
    }

    const errStart = ERROR_START.exec(line);
    if (errStart) {
      pendingError = errStart[1].trim();
      errors.push(pendingError);
      continue;
    }

    const errWith = ERROR_WITH.exec(line);
    if (errWith && pendingError !== undefined) {
      const address = errWith[1];
      const current = resources.get(address) ?? { address, action: "create" as const, state: "running" as const };
      resources.set(address, { ...current, state: "error", detail: pendingError });
      pendingError = undefined;
    }
  }

  return { resources: Array.from(resources.values()), errors };
};

export const hasApplyProgress = (log?: string): boolean => parseApplyProgress(log).resources.length > 0;
