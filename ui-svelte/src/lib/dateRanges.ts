/**
 * Helpers for the activity screen's predefined date ranges.
 *
 * All calculations are done in the *user's local timezone* so that
 * "Today" means "from midnight local time" regardless of where the
 * browser sits relative to the server. The returned values are
 * millisecond timestamps suitable for converting to ISO strings.
 */

/**
 * Midnight (00:00:00.000) of the given date in local time.
 * Defaults to the current day when no reference is supplied.
 */
export function startOfToday(now: Date = new Date()): number {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
}

/**
 * Midnight (00:00:00.000) of the most recent Monday on or before the
 * given date, in local time. Defaults to the current day.
 *
 * `Date.getDay()` returns 0 for Sunday, so the Monday offset is
 * `(day + 6) % 7` days back.
 */
export function startOfWeek(now: Date = new Date()): number {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  const day = d.getDay(); // 0 = Sunday ... 6 = Saturday
  const offset = (day + 6) % 7; // days since Monday
  d.setDate(d.getDate() - offset);
  return d.getTime();
}
