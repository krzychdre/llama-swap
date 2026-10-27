<script lang="ts">
  import type { Model } from "../lib/types";
  import { sleepModel, wakeModel } from "../stores/api";
  import { Moon, Sun, Loader2 } from "@lucide/svelte";

  interface Props {
    model: Model;
    /** "md" for list rows (size-7), "sm" for the detail header (size-5). */
    size?: "md" | "sm";
  }

  let { model, size = "md" }: Props = $props();

  let btnSize = $derived(size === "sm" ? "size-5 rounded-sm" : "size-7 rounded-md");
  let iconSize = $derived(size === "sm" ? "size-3.5" : "size-4");

  let transitioning = $derived(model.state === "waking" || model.state === "going-to-sleep");
  let canSleep = $derived(model.state === "ready");
  let canWake = $derived(model.state === "sleeping");

  function onClick(): void {
    if (canSleep) {
      sleepModel(model.id);
    } else if (canWake) {
      wakeModel(model.id);
    }
  }
</script>

{#if model.sleepWakeEnabled && (canSleep || canWake || transitioning)}
  <button
    type="button"
    class="text-muted-foreground hover:bg-accent hover:text-accent-foreground flex {btnSize} shrink-0 items-center justify-center disabled:opacity-50"
    title={canSleep ? "Sleep" : canWake ? "Wake" : "Busy"}
    aria-label={canSleep ? "Sleep model" : "Wake model"}
    disabled={transitioning}
    onclick={onClick}
  >
    {#if transitioning}
      <Loader2 class="{iconSize} animate-spin" />
    {:else if canWake}
      <Sun class={iconSize} />
    {:else}
      <Moon class={iconSize} />
    {/if}
  </button>
{/if}
