<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { inflightRequestEntries, metrics } from "../../stores/api";
  import { MetricsPager } from "../../lib/metricsPager.svelte";
  import { persistentStore } from "../../stores/persistent";
  import ActivityTable from "../ActivityTable.svelte";

  interface Props {
    modelId: string;
  }

  let { modelId }: Props = $props();

  const storedPageSize = persistentStore<number>("model-detail-page-size", 10);

  // svelte-ignore state_referenced_locally
  const pager = new MetricsPager({ pageSize: $storedPageSize, filters: { model: modelId } });

  $effect(() => {
    storedPageSize.set(pager.pageSize);
  });

  onMount(() => {
    pager.first();
  });

  // Refetch when the route's model changes without a remount.
  $effect(() => {
    const id = modelId;
    untrack(() => {
      if (pager.filters.model !== id) pager.setFilters({ model: id });
    });
  });

  $effect(() => {
    const live = $metrics;
    untrack(() => {
      if (live.length > 0) pager.handleLive(live);
    });
  });

  let modelInflightRequests = $derived(
    $inflightRequestEntries.filter((request) => request.model === modelId)
  );
</script>

<ActivityTable
  metrics={pager.entries}
  {pager}
  inflightRequests={modelInflightRequests}
  storagePrefix="model-detail"
  showModelColumn={false}
  showPagination={true}
  compact={true}
  title="Recent Activity"
  emptyMessage="No activity recorded for this model"
/>
