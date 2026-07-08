<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { fetchMetricsSummary, inflightRequestEntries, metrics } from "../stores/api";
  import type { MetricsSummary } from "../lib/types";
  import { MetricsPager, type MetricsFilters } from "../lib/metricsPager.svelte";
  import { DEFAULT_PRESET, presetFilters } from "../lib/activityPresets";
  import { persistentStore } from "../stores/persistent";
  import ActivityStats from "../components/ActivityStats.svelte";
  import ActivityTable from "../components/ActivityTable.svelte";
  import ActivityFilters from "../components/ActivityFilters.svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { RefreshCw } from "@lucide/svelte";

  const storedPageSize = persistentStore<number>("activity-page-size", 10);

  // Seed the pager with the default preset ("Today") so the initial load
  // matches the dropdown's default selection instead of silently using
  // "All time".
  // svelte-ignore state_referenced_locally
  const pager = new MetricsPager({
    pageSize: $storedPageSize,
    filters: presetFilters(DEFAULT_PRESET) as MetricsFilters,
  });
  let summary = $state<MetricsSummary | null>(null);

  $effect(() => {
    storedPageSize.set(pager.pageSize);
  });

  const SUMMARY_REFRESH_MS = 5000;
  let summaryTimer: ReturnType<typeof setTimeout> | null = null;
  let lastSummaryAt = 0;

  async function loadSummary() {
    lastSummaryAt = Date.now();
    summary = await fetchMetricsSummary({
      from: pager.filters.from,
      to: pager.filters.to,
      model: pager.filters.model,
    });
  }

  // Live entries invalidate the aggregates; refetch at most every 5s.
  function scheduleSummaryRefresh() {
    if (summaryTimer) return;
    const wait = Math.max(0, SUMMARY_REFRESH_MS - (Date.now() - lastSummaryAt));
    summaryTimer = setTimeout(() => {
      summaryTimer = null;
      loadSummary();
    }, wait);
  }

  async function applyFilters(filters: MetricsFilters) {
    await pager.setFilters(filters);
    await loadSummary();
  }

  function refresh() {
    pager.first();
    loadSummary();
  }

  onMount(() => {
    pager.first();
    loadSummary();
    return () => {
      if (summaryTimer) clearTimeout(summaryTimer);
    };
  });

  $effect(() => {
    const live = $metrics;
    untrack(() => {
      if (live.length === 0) return;
      pager.handleLive(live);
      scheduleSummaryRefresh();
    });
  });
</script>

<div class="p-2">
  <div class="mt-4 mb-4">
    <ActivityStats {summary} />
  </div>

  <div class="mb-3 flex flex-wrap items-center justify-between gap-2">
    <ActivityFilters onchange={applyFilters} />
    {#if pager.pending > 0}
      <Button variant="outline" size="sm" class="h-7 text-xs" onclick={refresh}>
        <RefreshCw class="size-3.5" />
        {pager.pending} new {pager.pending === 1 ? "entry" : "entries"} — refresh
      </Button>
    {/if}
  </div>

  <ActivityTable
    metrics={pager.entries}
    {pager}
    inflightRequests={$inflightRequestEntries}
    storagePrefix="activity"
    showModelColumn={true}
    showPagination={true}
    cardClass="min-h-[30rem] overflow-auto"
    emptyMessage="No activity recorded"
  />
</div>
