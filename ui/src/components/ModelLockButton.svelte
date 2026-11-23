<script lang="ts">
  import type { Model } from "../lib/types";
  import { setModelLocked } from "../stores/api";
  import { Lock, LockOpen } from "@lucide/svelte";

  interface Props {
    model: Model;
  }

  let { model }: Props = $props();
</script>

<button
  type="button"
  class="hover:bg-accent hover:text-accent-foreground flex size-7 shrink-0 items-center justify-center rounded-md {model.locked
    ? 'text-warning'
    : 'text-muted-foreground'}"
  title={model.locked
    ? "Locked: requests for other models cannot unload it. Click to unlock"
    : "Lock: keep requests for other models from unloading it"}
  aria-label={model.locked ? "Unlock model" : "Lock model"}
  aria-pressed={model.locked === true}
  onclick={() => setModelLocked(model.id, !model.locked)}
>
  {#if model.locked}
    <Lock class="size-4" />
  {:else}
    <LockOpen class="size-4" />
  {/if}
</button>
