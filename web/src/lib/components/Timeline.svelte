<script lang="ts">
	import type { RunEvent } from '$lib/api';

	let { events = [] }: { events?: RunEvent[] } = $props();

	function time(value: string): string {
		const parsed = new Date(value);
		if (Number.isNaN(parsed.getTime())) {
			return value;
		}
		return parsed.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
	}
</script>

{#if events.length === 0}
	<p class="empty" data-testid="timeline-empty">Nothing has happened yet.</p>
{:else}
	<ol class="timeline" data-testid="timeline">
		{#each events as event (event.seq)}
			<li class={event.severity}>
				<span class="at">{time(event.at)}</span>
				<span class="kind">{event.type}</span>
				<span class="message">{event.message}</span>
			</li>
		{/each}
	</ol>
{/if}

<style>
	.timeline {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		max-height: 22rem;
		overflow: auto;
		font-size: 0.86rem;
	}

	li {
		display: grid;
		grid-template-columns: 5.5rem 5rem 1fr;
		gap: 0.6rem;
		padding: 0.3rem 0.4rem;
		border-left: 3px solid #dbe2ea;
		background: #fbfcfd;
	}

	li.warn {
		border-left-color: #d9a441;
		background: #fdf9ee;
	}

	li.error {
		border-left-color: #b3261e;
		background: #fdf1f0;
	}

	.at {
		color: #6b7a8c;
		font-variant-numeric: tabular-nums;
	}

	.kind {
		color: #5b6b7f;
		text-transform: uppercase;
		font-size: 0.7rem;
		letter-spacing: 0.04em;
		padding-top: 0.15rem;
	}

	.message {
		color: #1b1f24;
		word-break: break-word;
	}

	.empty {
		margin: 0;
		color: #6b7a8c;
		font-size: 0.88rem;
	}
</style>
