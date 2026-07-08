import { startOfToday, startOfWeek } from "./dateRanges";

export interface ActivityPreset {
  id: string;
  label: string;
}

/** Ordered list of selectable range presets shown in the activity filters. */
export const ACTIVITY_PRESETS: ActivityPreset[] = [
  { id: "all", label: "All time" },
  { id: "today", label: "Today" },
  { id: "week", label: "This week" },
  { id: "1h", label: "Last hour" },
  { id: "24h", label: "Last 24 hours" },
  { id: "7d", label: "Last 7 days" },
  { id: "30d", label: "Last 30 days" },
  { id: "custom", label: "Custom range" },
];

/** Preset selected by default when the activity screen opens. */
export const DEFAULT_PRESET = "today";

// Rolling-window presets expressed as a duration in milliseconds.
const PRESET_MS: Record<string, number> = {
  "1h": 3600_000,
  "24h": 24 * 3600_000,
  "7d": 7 * 24 * 3600_000,
  "30d": 30 * 24 * 3600_000,
};

// Boundary presets computed from a fixed local-time anchor (midnight).
const PRESET_FROM: Record<string, () => number> = {
  today: startOfToday,
  week: startOfWeek,
};

export interface PresetFiltersOptions {
  customFrom?: string;
  customTo?: string;
  model?: string;
}

/**
 * Resolve a preset id into the metrics filters it represents.
 *
 * `from`/`to` are returned as RFC3339 strings (or `undefined` when the
 * preset implies no bound, e.g. "all time"). Keeping this resolution in
 * one place lets both the filter UI and the pager's initial load agree
 * on what a given preset means.
 */
export function presetFilters(
  preset: string,
  { customFrom, customTo, model }: PresetFiltersOptions = {},
): { from?: string; to?: string; model?: string } {
  let from: string | undefined;
  let to: string | undefined;
  if (preset === "custom") {
    if (customFrom) from = new Date(customFrom).toISOString();
    if (customTo) to = new Date(customTo).toISOString();
  } else if (PRESET_FROM[preset]) {
    from = new Date(PRESET_FROM[preset]()).toISOString();
  } else if (preset !== "all") {
    from = new Date(Date.now() - PRESET_MS[preset]).toISOString();
  }
  return { from, to, model: model || undefined };
}
