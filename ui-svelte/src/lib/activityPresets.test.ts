import { describe, it, expect } from "vitest";
import {
  ACTIVITY_PRESETS,
  DEFAULT_PRESET,
  presetFilters,
} from "./activityPresets";

describe("ACTIVITY_PRESETS", () => {
  it("includes the default preset id", () => {
    expect(ACTIVITY_PRESETS.some((p) => p.id === DEFAULT_PRESET)).toBe(true);
  });

  it("has unique ids", () => {
    const ids = ACTIVITY_PRESETS.map((p) => p.id);
    expect(new Set(ids).size).toBe(ids.length);
  });
});

describe("DEFAULT_PRESET", () => {
  it("is 'today'", () => {
    expect(DEFAULT_PRESET).toBe("today");
  });
});

describe("presetFilters", () => {
  it("returns no bounds for 'all'", () => {
    expect(presetFilters("all")).toEqual({});
  });

  it("returns undefined model when none is provided", () => {
    expect(presetFilters("all", { model: "" })).toEqual({ model: undefined });
  });

  it("passes through the model filter", () => {
    expect(presetFilters("all", { model: "llama" })).toEqual({
      model: "llama",
    });
  });

  it("resolves 'today' to midnight local time", () => {
    const now = new Date();
    const midnight = new Date(now);
    midnight.setHours(0, 0, 0, 0);
    const { from } = presetFilters("today");
    expect(from).toBe(midnight.toISOString());
  });

  it("resolves 'week' to the most recent Monday midnight", () => {
    const now = new Date();
    const monday = new Date(now);
    monday.setHours(0, 0, 0, 0);
    monday.setDate(monday.getDate() - ((monday.getDay() + 6) % 7));
    const { from } = presetFilters("week");
    expect(from).toBe(monday.toISOString());
  });

  it("resolves rolling-window presets to a duration back from now", () => {
    const before = Date.now();
    const { from } = presetFilters("1h");
    const after = Date.now();
    expect(from).toBeDefined();
    const ms = new Date(from!).getTime();
    expect(ms).toBeGreaterThanOrEqual(before - 3600_000 - 1);
    expect(ms).toBeLessThanOrEqual(after - 3600_000);
  });

  it("resolves custom from/to inputs to RFC3339 strings", () => {
    const { from, to } = presetFilters("custom", {
      customFrom: "2026-06-15T14:30",
      customTo: "2026-06-16T09:00",
    });
    expect(from).toBe(new Date("2026-06-15T14:30").toISOString());
    expect(to).toBe(new Date("2026-06-16T09:00").toISOString());
  });

  it("omits custom bounds that are empty", () => {
    const { from, to } = presetFilters("custom", {
      customFrom: "",
      customTo: "",
    });
    expect(from).toBeUndefined();
    expect(to).toBeUndefined();
  });
});
