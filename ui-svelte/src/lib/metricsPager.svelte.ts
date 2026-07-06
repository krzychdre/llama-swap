import type { ActivityLogEntry, MetricsQuery } from "./types";
import { fetchMetrics } from "../stores/api";

export interface MetricsFilters {
  from?: string; // RFC3339
  to?: string; // RFC3339
  model?: string;
}

/**
 * Server-side pager over /api/metrics using keyset pagination (before_id).
 * Live SSE entries are merged in-place on the newest page when no date filter
 * is active; otherwise they accumulate in `pending` for a manual refresh.
 */
export class MetricsPager {
  entries = $state<ActivityLogEntry[]>([]);
  total = $state(0);
  hasMore = $state(false);
  pageIndex = $state(0);
  pageSize = $state(10);
  pending = $state(0);
  loading = $state(false);
  filters = $state<MetricsFilters>({});

  /* cursors[i] is the before_id used to load page i (page 0 has none). */
  private cursors: (number | undefined)[] = [undefined];
  private lastLiveId = 0;

  constructor(init?: { pageSize?: number; filters?: MetricsFilters }) {
    if (init?.pageSize) this.pageSize = init.pageSize;
    if (init?.filters) this.filters = init.filters;
  }

  private query(beforeId?: number): MetricsQuery {
    return {
      limit: this.pageSize,
      before_id: beforeId,
      from: this.filters.from,
      to: this.filters.to,
      model: this.filters.model,
    };
  }

  private async load(beforeId?: number): Promise<void> {
    this.loading = true;
    try {
      const page = await fetchMetrics(this.query(beforeId));
      if (!page) return;
      this.entries = page.entries;
      this.total = page.total;
      this.hasMore = page.has_more;
    } finally {
      this.loading = false;
    }
  }

  async first(): Promise<void> {
    this.pageIndex = 0;
    this.cursors = [undefined];
    this.pending = 0;
    await this.load(undefined);
  }

  async next(): Promise<void> {
    if (!this.hasMore || this.entries.length === 0) return;
    const cursor = this.entries[this.entries.length - 1].id;
    this.cursors = [...this.cursors.slice(0, this.pageIndex + 1), cursor];
    this.pageIndex += 1;
    await this.load(cursor);
  }

  async prev(): Promise<void> {
    if (this.pageIndex === 0) return;
    this.pageIndex -= 1;
    await this.load(this.cursors[this.pageIndex]);
  }

  async setPageSize(size: number): Promise<void> {
    this.pageSize = size;
    await this.first();
  }

  async setFilters(filters: MetricsFilters): Promise<void> {
    this.filters = filters;
    await this.first();
  }

  /**
   * Merge live SSE entries. On the newest page without a date filter the
   * entries are prepended directly (keyset cursors for older pages stay
   * valid); anywhere else they only bump `pending`.
   */
  handleLive(live: ActivityLogEntry[]): void {
    const fresh = live.filter((m) => m.id > this.lastLiveId);
    if (fresh.length === 0) return;
    this.lastLiveId = Math.max(this.lastLiveId, ...fresh.map((m) => m.id));

    const matching = this.filters.model
      ? fresh.filter((m) => m.model === this.filters.model)
      : fresh;
    if (matching.length === 0) return;

    const hasDateFilter = !!(this.filters.from || this.filters.to);
    if (this.pageIndex === 0 && !hasDateFilter) {
      const sorted = [...matching].sort((a, b) => b.id - a.id);
      this.entries = [...sorted, ...this.entries].slice(0, this.pageSize);
      this.total += matching.length;
      if (!this.hasMore && this.total > this.pageSize) this.hasMore = true;
    } else {
      this.pending += matching.length;
    }
  }
}
