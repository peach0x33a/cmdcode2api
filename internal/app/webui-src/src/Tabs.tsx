export type TabId = "accounts" | "models" | "usage" | "monitor" | "alerts" | "setup";

const TAB_LABELS: Record<TabId, string> = {
  accounts: "Accounts",
  models: "Models",
  usage: "Usage",
  monitor: "Monitoring",
  alerts: "Alerts",
  setup: "Setup",
};

export const TAB_ORDER: TabId[] = ["accounts", "models", "usage", "monitor", "alerts", "setup"];

export interface TabsProps {
  active: TabId;
  onSelect: (id: TabId) => void;
}

export default function Tabs({ active, onSelect }: TabsProps) {
  return (
    <div className="tabs" role="tablist">
      {TAB_ORDER.map((id) => (
        <button
          key={id}
          role="tab"
          aria-selected={active === id}
          className={`tab${active === id ? " active" : ""}`}
          onClick={() => onSelect(id)}
        >
          {TAB_LABELS[id]}
        </button>
      ))}
    </div>
  );
}
