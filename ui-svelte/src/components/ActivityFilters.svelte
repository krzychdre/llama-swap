<script lang="ts">
  import { models } from "../stores/api";
  import type { MetricsFilters } from "../lib/metricsPager.svelte";
  import * as Select from "$lib/components/ui/select/index.js";
  import { Input } from "$lib/components/ui/input/index.js";

  interface Props {
    onchange: (filters: MetricsFilters) => void;
  }

  let { onchange }: Props = $props();

  const PRESETS = [
    { id: "all", label: "All time" },
    { id: "1h", label: "Last hour" },
    { id: "24h", label: "Last 24 hours" },
    { id: "7d", label: "Last 7 days" },
    { id: "30d", label: "Last 30 days" },
    { id: "custom", label: "Custom range" },
  ];
  const PRESET_MS: Record<string, number> = {
    "1h": 3600_000,
    "24h": 24 * 3600_000,
    "7d": 7 * 24 * 3600_000,
    "30d": 30 * 24 * 3600_000,
  };

  let preset = $state("all");
  let model = $state("");
  let customFrom = $state("");
  let customTo = $state("");

  let presetLabel = $derived(PRESETS.find((p) => p.id === preset)?.label ?? "All time");
  let modelIds = $derived([...new Set($models.map((m) => m.id))].sort());

  function emit() {
    let from: string | undefined;
    let to: string | undefined;
    if (preset === "custom") {
      if (customFrom) from = new Date(customFrom).toISOString();
      if (customTo) to = new Date(customTo).toISOString();
    } else if (preset !== "all") {
      from = new Date(Date.now() - PRESET_MS[preset]).toISOString();
    }
    onchange({ from, to, model: model || undefined });
  }
</script>

<div class="flex flex-wrap items-center gap-2 text-sm">
  <span class="text-muted-foreground text-xs uppercase tracking-wider">Range</span>
  <Select.Root
    type="single"
    value={preset}
    onValueChange={(v) => {
      preset = v;
      emit();
    }}
  >
    <Select.Trigger size="sm" class="h-7 w-[9.5rem] text-xs">
      {presetLabel}
    </Select.Trigger>
    <Select.Content>
      {#each PRESETS as p (p.id)}
        <Select.Item value={p.id}>{p.label}</Select.Item>
      {/each}
    </Select.Content>
  </Select.Root>

  {#if preset === "custom"}
    <Input
      type="datetime-local"
      class="h-7 w-[13rem] text-xs"
      bind:value={customFrom}
      onchange={emit}
      aria-label="From"
    />
    <span class="text-muted-foreground text-xs">to</span>
    <Input
      type="datetime-local"
      class="h-7 w-[13rem] text-xs"
      bind:value={customTo}
      onchange={emit}
      aria-label="To"
    />
  {/if}

  <span class="text-muted-foreground ml-2 text-xs uppercase tracking-wider">Model</span>
  <Select.Root
    type="single"
    value={model}
    onValueChange={(v) => {
      model = v;
      emit();
    }}
  >
    <Select.Trigger size="sm" class="h-7 max-w-[16rem] text-xs">
      <span class="truncate">{model || "All models"}</span>
    </Select.Trigger>
    <Select.Content>
      <Select.Item value="">All models</Select.Item>
      {#each modelIds as id (id)}
        <Select.Item value={id}>{id}</Select.Item>
      {/each}
    </Select.Content>
  </Select.Root>
</div>
