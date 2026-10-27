<script lang="ts">
  import { untrack } from "svelte";
  import type { ActivityLogEntry, ActivityStatsData } from "../lib/types";
  import { activityRevision, getActivity, getActivityStats, inflightRequestEntries } from "../stores/api";
  import { connectionState } from "../stores/theme";
  import { persistentStore } from "../stores/persistent";
  import {
    emptyActivityFilters,
    normalizeActivityFilters,
    type ActivityFilters,
  } from "../lib/activityFilters";
  import { DEFAULT_PRESET, presetFilters, type ActivityRange } from "../lib/activityPresets";
  import ActivityStats from "../components/ActivityStats.svelte";
  import ActivityTable from "../components/ActivityTable.svelte";
  import ActivityRangeFilters from "../components/ActivityRangeFilters.svelte";

  const storedPageSize = persistentStore<number>("activity-page-size", 25);
  const storedFilters = persistentStore<ActivityFilters>(
    "activity-filters",
    emptyActivityFilters()
  );

  let rows = $state<ActivityLogEntry[]>([]);
  let stats = $state<ActivityStatsData | null>(null);
  let page = $state(1);
  let limit = $state($storedPageSize);
  let sort = $state("id");
  let order = $state<"asc" | "desc">("desc");
  let total = $state(0);
  let totalPages = $state(0);
  // svelte-ignore state_referenced_locally
  let filters = $state<ActivityFilters>(normalizeActivityFilters($storedFilters));
  // Seeded with the default preset so the first load matches the range
  // dropdown's initial selection.
  let range = $state<ActivityRange>(presetFilters(DEFAULT_PRESET));
  let requestID = 0;
  let refreshTimer: ReturnType<typeof setTimeout> | null = null;
  let lastRefresh = 0;

  async function refreshActivity() {
    if (refreshTimer !== null) {
      clearTimeout(refreshTimer);
      refreshTimer = null;
    }
    lastRefresh = Date.now();
    const id = ++requestID;
    try {
      const rangeParams = { model: range.model, start: range.from, end: range.to };
      const [activity, activityStats] = await Promise.all([
        getActivity({ ...rangeParams, page, limit, sort, order, filters }),
        // Stats follow the range and model, not the drawer's id bounds.
        getActivityStats(rangeParams),
      ]);
      if (id !== requestID) return;
      rows = activity.data;
      total = activity.total;
      totalPages = activity.total_pages;
      stats = activityStats;
    } catch (error) {
      console.error("Failed to refresh activity:", error);
    }
  }

  function setPage(nextPage: number) {
    page = nextPage;
  }

  function setPageSize(nextLimit: number) {
    limit = nextLimit;
    page = 1;
    storedPageSize.set(nextLimit);
  }

  function setSort(nextSort: string, nextOrder: "asc" | "desc") {
    sort = nextSort;
    order = nextOrder;
    page = 1;
  }

  function setRange(nextRange: ActivityRange) {
    range = nextRange;
    page = 1;
  }

  function setFilters(nextFilters: ActivityFilters) {
    filters = nextFilters;
    page = 1;
    storedFilters.set(nextFilters);
  }

  // scheduleRefresh throttles SSE-driven refreshes to one per second; a
  // user-driven refreshActivity cancels any pending timer.
  function scheduleRefresh() {
    if (refreshTimer !== null) return;
    const wait = Math.max(0, 1000 - (Date.now() - lastRefresh));
    refreshTimer = setTimeout(() => {
      refreshTimer = null;
      refreshActivity();
    }, wait);
  }

  // Refresh immediately on connect and when the user changes paging/sorting.
  $effect(() => {
    if ($connectionState !== "connected") return;
    page;
    limit;
    sort;
    order;
    filters;
    range;
    untrack(() => {
      refreshActivity();
    });
  });

  // New activity only changes what page 1 shows; skip SSE-driven refreshes
  // while browsing older pages. seenRevision keeps the effect's initial run
  // (and re-runs from other deps) from scheduling a redundant fetch.
  let seenRevision = $activityRevision;
  $effect(() => {
    if ($connectionState !== "connected") return;
    const revision = $activityRevision;
    untrack(() => {
      if (revision === seenRevision) return;
      seenRevision = revision;
      if (page === 1) scheduleRefresh();
    });
  });

  $effect(() => {
    return () => {
      if (refreshTimer !== null) clearTimeout(refreshTimer);
    };
  });
</script>

<div class="p-2">
  <div class="mt-4 mb-4">
    <ActivityStats {stats} />
  </div>

  <div class="mb-3">
    <ActivityRangeFilters onchange={setRange} />
  </div>

  <ActivityTable
    metrics={rows}
    inflightRequests={$inflightRequestEntries}
    storagePrefix="activity"
    showModelColumn={true}
    showPagination={true}
    {page}
    {limit}
    {total}
    totalPages={totalPages}
    onPageChange={setPage}
    onPageSizeChange={setPageSize}
    {sort}
    {order}
    onSortChange={setSort}
    {filters}
    onFiltersChange={setFilters}
    cardClass="min-h-[30rem] overflow-auto"
    emptyMessage="No activity recorded"
  />
</div>
