import {
  CheckCircleFilled,
  ClockCircleOutlined,
  CloseCircleFilled,
  CopyOutlined,
  DownOutlined,
  FilterOutlined,
  LoadingOutlined,
  RightOutlined,
} from "@ant-design/icons";
import { Empty, message } from "antd";
import type { ChangeEvent, ReactNode } from "react";
import { useMemo, useState } from "react";
import { ApplyResource, ApplyResourceState, PlannedResource, parseApplyProgress } from "./applyProgressParser";
import { actionMeta, getProviderLabel, getProviderName, providerIconMap } from "./StructuredPlanOutput";
import "./StructuredPlanOutput.css";
import "./ApplyProgress.css";

type Props = {
  outputLog?: string;
  isRunning: boolean;
  planned?: PlannedResource[];
};

type StatusFilter = "all" | ApplyResourceState;

const STATE_META: Record<ApplyResourceState, { label: string; summaryLabel: string; icon: ReactNode }> = {
  pending: { label: "Pending", summaryLabel: "pending", icon: <ClockCircleOutlined /> },
  running: { label: "In progress", summaryLabel: "in progress", icon: <LoadingOutlined spin /> },
  done: { label: "Complete", summaryLabel: "complete", icon: <CheckCircleFilled /> },
  error: { label: "Failed", summaryLabel: "failed", icon: <CloseCircleFilled /> },
};

// Order of the summary strip, most urgent first.
const SUMMARY_ORDER: ApplyResourceState[] = ["error", "running", "done", "pending"];

export const ApplyProgress = ({ outputLog, isRunning, planned }: Props) => {
  const [expanded, setExpanded] = useState<string[]>([]);
  const [addressFilter, setAddressFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");

  const { resources } = useMemo(() => parseApplyProgress(outputLog, planned), [outputLog, planned]);

  // A step that stopped running leaves unfinished resources interrupted, not "in progress".
  const rows = useMemo(
    () =>
      resources.map((r) => ({
        ...r,
        state: (r.state === "running" && !isRunning ? "error" : r.state) as ApplyResourceState,
      })),
    [resources, isRunning]
  );

  const counts = useMemo(() => {
    const c: Record<ApplyResourceState, number> = { pending: 0, running: 0, done: 0, error: 0 };
    rows.forEach((r) => {
      c[r.state] += 1;
    });
    return c;
  }, [rows]);

  const filtered = useMemo(() => {
    const needle = addressFilter.trim().toLowerCase();
    return rows.filter(
      (r) =>
        (statusFilter === "all" || r.state === statusFilter) && (!needle || r.address.toLowerCase().includes(needle))
    );
  }, [rows, addressFilter, statusFilter]);

  const toggle = (address: string) =>
    setExpanded((cur) => (cur.includes(address) ? cur.filter((a) => a !== address) : [...cur, address]));

  const copy = async (address: string) => {
    try {
      await navigator.clipboard.writeText(address);
      message.success("Address copied");
    } catch {
      message.error("Failed to copy address");
    }
  };

  const renderRow = (r: ApplyResource & { state: ApplyResourceState }) => {
    const isOpen = expanded.includes(r.address);
    const meta = actionMeta[r.action];
    const providerName = getProviderName({ address: r.address });
    const ProviderIcon = providerIconMap[providerName.toLowerCase() as keyof typeof providerIconMap];

    return (
      <div key={r.address} className={`structured-plan-row apply-progress-row apply-progress-row--${r.state}`}>
        <div className="structured-plan-rowHeader">
          <button
            aria-expanded={isOpen}
            className="structured-plan-rowToggle"
            onClick={() => toggle(r.address)}
            type="button"
          >
            <span className={`structured-plan-chevron${isOpen ? " structured-plan-chevron--expanded" : ""}`}>
              <RightOutlined />
            </span>
            <span
              aria-hidden="true"
              className={`structured-plan-actionIcon structured-plan-actionIcon--${meta.className}`}
              title={meta.displayLabel}
            >
              {meta.symbol}
            </span>
            <span className="structured-plan-providerBadge" title={providerName}>
              {ProviderIcon ? (
                <ProviderIcon className="structured-plan-providerBadgeIcon" />
              ) : (
                <span>{getProviderLabel(providerName)}</span>
              )}
            </span>
            <span className="structured-plan-address" title={r.address}>
              {r.address}
            </span>
            <span className={`apply-progress-status apply-progress-status--${r.state}`}>
              {STATE_META[r.state].icon}
              <span>{STATE_META[r.state].label}</span>
              {r.detail && r.state !== "pending" ? (
                <span className="apply-progress-statusDetail">{r.detail}</span>
              ) : null}
            </span>
          </button>
          <button
            aria-label="Copy resource address"
            className="structured-plan-copyButton"
            onClick={() => void copy(r.address)}
            title="Copy resource address"
            type="button"
          >
            <CopyOutlined />
          </button>
        </div>
        {isOpen ? (
          <div className="structured-plan-rowBody">
            <div className="structured-plan-rowBodyInner">
              {r.log.length > 0 ? (
                <pre className="apply-progress-log">{r.log.join("\n")}</pre>
              ) : (
                <div className="structured-plan-emptyState">
                  <Empty description="Not started yet." image={Empty.PRESENTED_IMAGE_SIMPLE} />
                </div>
              )}
            </div>
          </div>
        ) : null}
      </div>
    );
  };

  const total = rows.length;
  const isFiltered = statusFilter !== "all" || addressFilter.trim() !== "";

  return (
    <div className="structured-plan">
      <div className="structured-plan-shell">
        <div className="structured-plan-summaryArea">
          <div className="structured-plan-summaryStrip">
            {SUMMARY_ORDER.filter((s) => counts[s] > 0).map((s) => (
              <div
                key={s}
                className={`structured-plan-summarySegment apply-progress-segment--${s}`}
                style={{ flexGrow: counts[s] }}
              >
                <span className="structured-plan-summarySymbol">{STATE_META[s].icon}</span>
                <span>
                  {counts[s]} {STATE_META[s].summaryLabel}
                </span>
              </div>
            ))}
          </div>
          <div className="structured-plan-summaryMeta">
            <span>
              Resources: {counts.done} of {total} complete
            </span>
          </div>
        </div>

        <div className="structured-plan-toolbar">
          <div className="structured-plan-toolbarControls">
            <input
              aria-label="Filter resources by address"
              className="structured-plan-searchInput"
              onChange={(e: ChangeEvent<HTMLInputElement>) => setAddressFilter(e.target.value)}
              placeholder="Filter resources by address..."
              type="text"
              value={addressFilter}
            />
            <label className="structured-plan-selectWrap">
              <FilterOutlined className="structured-plan-selectIcon" />
              <select
                aria-label="Filter by status"
                className="structured-plan-select"
                onChange={(e: ChangeEvent<HTMLSelectElement>) => setStatusFilter(e.target.value as StatusFilter)}
                value={statusFilter}
              >
                <option value="all">All statuses</option>
                <option value="done">Complete</option>
                <option value="running">In progress</option>
                <option value="error">Failed</option>
                <option value="pending">Pending</option>
              </select>
              <DownOutlined className="structured-plan-selectChevron" />
            </label>
          </div>
          <div className="structured-plan-toolbarMeta">
            {isFiltered ? (
              <span>
                Showing {filtered.length} of {total}
              </span>
            ) : null}
          </div>
        </div>

        <div className="structured-plan-rows">
          {filtered.length ? (
            filtered.map(renderRow)
          ) : (
            <div className="structured-plan-emptyState">
              <Empty description="No resources match the current filters." image={Empty.PRESENTED_IMAGE_SIMPLE} />
            </div>
          )}
        </div>
      </div>
    </div>
  );
};
