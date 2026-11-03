import { CheckCircleFilled, ClockCircleOutlined, CloseCircleFilled, LoadingOutlined } from "@ant-design/icons";
import { useMemo } from "react";
import { ApplyResource, PlannedResource, parseApplyProgress } from "./applyProgressParser";
import "./ApplyProgress.css";

type Props = {
  outputLog?: string;
  isRunning: boolean;
  planned?: PlannedResource[];
};

const ACTION_LABEL: Record<ApplyResource["action"], string> = {
  create: "Create",
  update: "Update",
  delete: "Destroy",
  replace: "Replace",
  read: "Read",
};

const STATE_ICON = {
  pending: <ClockCircleOutlined />,
  running: <LoadingOutlined spin />,
  done: <CheckCircleFilled />,
  error: <CloseCircleFilled />,
};

export const ApplyProgress = ({ outputLog, isRunning, planned }: Props) => {
  const { resources, errors } = useMemo(() => parseApplyProgress(outputLog, planned), [outputLog, planned]);

  const counts = useMemo(() => {
    return resources.reduce(
      (acc, r) => {
        acc[r.state] += 1;
        return acc;
      },
      { pending: 0, running: 0, done: 0, error: 0 }
    );
  }, [resources]);

  return (
    <div className="apply-progress">
      <div className="apply-progress-summary">
        <span className="apply-progress-chip apply-progress-done">{counts.done} complete</span>
        <span className="apply-progress-chip apply-progress-running">{counts.running} in progress</span>
        <span className="apply-progress-chip apply-progress-error">{counts.error} failed</span>
        {counts.pending > 0 && (
          <span className="apply-progress-chip apply-progress-idle">{counts.pending} pending</span>
        )}
      </div>
      <ul className="apply-progress-list">
        {resources.map((r) => {
          // A step that stopped running leaves unfinished resources interrupted, not "in progress".
          const state = r.state === "running" && !isRunning ? "error" : r.state;
          return (
            <li key={r.address} className={`apply-progress-row apply-progress-row-${state}`}>
              <span className="apply-progress-icon">{STATE_ICON[state]}</span>
              <span className="apply-progress-address">{r.address}</span>
              <span className="apply-progress-action">{ACTION_LABEL[r.action]}</span>
              <span className="apply-progress-detail">{r.state === "error" ? r.detail : (r.detail ?? "")}</span>
            </li>
          );
        })}
      </ul>
      {errors.length > 0 && (
        <div className="apply-progress-errors">
          {errors.map((e, i) => (
            <div key={i}>{e}</div>
          ))}
        </div>
      )}
    </div>
  );
};
