import type { EventStorySummary } from "@/lib/api";
import { CATEGORY_LABELS, fieldLabel } from "@/lib/labels";

export interface ConsoleHeaderProps {
  category: string;
  field: string;
  currentStory?: EventStorySummary;
  currentField?: { total: number };
  realtimeState: "connected" | "reconnecting" | "connecting" | "offline";
  onlineUsers: string[];
  selectedIndex: number;
  filteredCount: number;
}

export function ConsoleHeader({
  category,
  field,
  currentStory,
  currentField,
  realtimeState,
  onlineUsers,
  selectedIndex,
  filteredCount,
}: ConsoleHeaderProps) {
  const isEventStory = category === "eventStory";
  return (
    <div className="main-header">
      <div>
        <h2>
          {CATEGORY_LABELS[category] || category} /{" "}
          {isEventStory
            ? currentStory?.eventName || currentStory?.eventNameJapanese || `Event #${field}`
            : fieldLabel(category, field)}
        </h2>
        <div className="realtime-meta" role="status" aria-live="polite">
          <span className={`realtime-status ${realtimeState}`}>
            <span className="realtime-status-dot" aria-hidden="true" />
            {realtimeState === "connected"
              ? "实时已连接"
              : realtimeState === "reconnecting"
              ? "实时重连中"
              : realtimeState === "connecting"
              ? "正在连接实时服务"
              : "实时服务离线"}
          </span>
          <span
            className="online-users"
            title={onlineUsers.length ? onlineUsers.join("、") : "当前没有其他在线用户"}
          >
            在线 {onlineUsers.length} 人{onlineUsers.length > 0 && `：${onlineUsers.join("、")}`}
          </span>
        </div>
      </div>
      <span className="count">
        {selectedIndex >= 0 ? `${selectedIndex + 1} / ` : ""}
        {filteredCount} 条
        {currentField && ` （共 ${currentField.total}）`}
      </span>
    </div>
  );
}
