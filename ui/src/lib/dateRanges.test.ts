import { describe, it, expect } from "vitest";
import { startOfToday, startOfWeek } from "./dateRanges";

describe("startOfToday", () => {
  it("returns midnight of the same local day", () => {
    const now = new Date(2026, 5, 15, 14, 30, 45, 250); // 2026-06-15 14:30:45.250
    const ms = startOfToday(now);
    const start = new Date(ms);
    expect(start.getFullYear()).toBe(2026);
    expect(start.getMonth()).toBe(5); // June (0-indexed)
    expect(start.getDate()).toBe(15);
    expect(start.getHours()).toBe(0);
    expect(start.getMinutes()).toBe(0);
    expect(start.getSeconds()).toBe(0);
    expect(start.getMilliseconds()).toBe(0);
  });

  it("defaults to the current time when no argument is given", () => {
    const ms = startOfToday();
    const now = Date.now();
    // Must be at or before now and no more than a day behind.
    expect(ms).toBeLessThanOrEqual(now);
    expect(now - ms).toBeLessThan(24 * 3600_000);
  });
});

describe("startOfWeek", () => {
  it("returns the same midnight when already a Monday", () => {
    const now = new Date(2026, 5, 15, 9, 0, 0, 0); // 2026-06-15 is a Monday
    expect(startOfWeek(now)).toBe(startOfToday(now));
  });

  it("rolls back to the previous Monday from mid-week", () => {
    // 2026-06-16 is a Tuesday -> Monday is 2026-06-15
    const now = new Date(2026, 5, 16, 23, 59, 0, 0);
    const ms = startOfWeek(now);
    const start = new Date(ms);
    expect(start.getDay()).toBe(1); // Monday
    expect(start.getDate()).toBe(15);
    expect(start.getHours()).toBe(0);
  });

  it("rolls back across the previous Sunday", () => {
    // 2026-06-21 is a Sunday -> Monday is 2026-06-15
    const now = new Date(2026, 5, 21, 3, 0, 0, 0);
    const start = new Date(startOfWeek(now));
    expect(start.getDay()).toBe(1);
    expect(start.getDate()).toBe(15);
  });

  it("rolls back across the month boundary", () => {
    // 2026-06-07 is a Sunday -> Monday is 2026-06-01
    const now = new Date(2026, 5, 7, 12, 0, 0, 0);
    const start = new Date(startOfWeek(now));
    expect(start.getMonth()).toBe(5); // still June
    expect(start.getDate()).toBe(1);
    expect(start.getDay()).toBe(1);
  });

  it("defaults to the current time when no argument is given", () => {
    const ms = startOfWeek();
    const now = Date.now();
    expect(ms).toBeLessThanOrEqual(now);
    // No more than a week behind.
    expect(now - ms).toBeLessThan(7 * 24 * 3600_000);
  });
});
