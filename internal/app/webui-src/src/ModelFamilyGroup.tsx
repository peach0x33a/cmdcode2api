import { useState } from "react";
import Toggle from "./Toggle";
import type { AdminModel } from "./types";

function formatContextLength(value: number): string {
  if (!value) return "—";
  return value.toLocaleString();
}

export interface ModelFamilyGroupProps {
  family: string;
  familyLabel: string;
  /** This family's member models, in catalog order. */
  models: AdminModel[];
  busyIds: Set<string>;
  busyFamilies: Set<string>;
  onToggleModel: (id: string, enabled: boolean) => void;
  onToggleFamily: (family: string, enabled: boolean) => void;
}

// ModelFamilyGroup renders one model family: either a single flat row (a
// family with exactly one member and no variant — nothing to group) or a
// header row (family label, "N of M enabled" count, a family-level toggle
// reflecting all-on/all-off/mixed, and an expand/collapse control) followed
// by one row per member model.
export default function ModelFamilyGroup({
  family,
  familyLabel,
  models,
  busyIds,
  busyFamilies,
  onToggleModel,
  onToggleFamily,
}: ModelFamilyGroupProps) {
  const [expanded, setExpanded] = useState(true);

  const memberRow = (m: AdminModel) => (
    <tr key={m.id} className="model-member-row">
      <td>
        {m.id}
        {m.variant ? <span className="pill variant-pill">{m.variant}</span> : null}
      </td>
      <td className="muted">{m.name || "—"}</td>
      <td>{formatContextLength(m.context_length)}</td>
      <td className="muted">{m.owned_by}</td>
      <td>
        <Toggle
          checked={!m.excluded}
          disabled={busyIds.has(m.id)}
          label={`Enable ${m.id}`}
          onChange={(next) => onToggleModel(m.id, next)}
        />
      </td>
    </tr>
  );

  if (models.length === 1 && !models[0].variant) {
    return <tbody>{memberRow(models[0])}</tbody>;
  }

  const enabledCount = models.filter((m) => !m.excluded).length;
  const allEnabled = enabledCount === models.length;
  const allDisabled = enabledCount === 0;
  const mixed = !allEnabled && !allDisabled;

  return (
    <tbody>
      <tr className="model-family-row">
        <td colSpan={4}>
          <button
            type="button"
            className="family-expand"
            onClick={() => setExpanded((e) => !e)}
            aria-expanded={expanded}
          >
            <span className={`family-chevron${expanded ? " open" : ""}`} aria-hidden="true" />
            <span className="family-label">{familyLabel}</span>
            <span className="family-count">
              {enabledCount} of {models.length} enabled
            </span>
          </button>
        </td>
        <td>
          <Toggle
            checked={allEnabled}
            mixed={mixed}
            disabled={busyFamilies.has(family)}
            label={`Enable all of ${familyLabel}`}
            onChange={(next) => onToggleFamily(family, next)}
          />
        </td>
      </tr>
      {expanded ? models.map(memberRow) : null}
    </tbody>
  );
}
