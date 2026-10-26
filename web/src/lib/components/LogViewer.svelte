<script lang="ts">
	import { ApiError, getRunLogs } from '$lib/api';

	let { runId }: { runId: string } = $props();

	let open = $state(false);
	let loading = $state(false);
	let loaded = $state(false);
	let pruned = $state(false);
	let error = $state('');
	let lines = $state<string[]>([]);

	async function load() {
		loading = true;
		error = '';
		pruned = false;
		try {
			lines = await getRunLogs(runId, 500);
			loaded = true;
		} catch (failure) {
			if (failure instanceof ApiError && failure.status === 410) {
				pruned = true;
				loaded = true;
			} else {
				error = failure instanceof ApiError ? failure.problem.detail : 'The log could not be read.';
			}
		} finally {
			loading = false;
		}
	}

	async function toggle() {
		open = !open;
		if (open && !loaded && !loading) {
			await load();
		}
	}
</script>

<div class="log-viewer">
	<button type="button" class="toggle" data-testid="logs-toggle" onclick={toggle} aria-expanded={open}>
		{open ? 'Hide logs' : 'Show logs'}
	</button>

	{#if open}
		<div class="body" data-testid="log-viewer">
			{#if loading}
				<p class="muted" role="status">Loading the redacted log…</p>
			{:else if pruned}
				<p class="muted">Logs were pruned for this run; the timeline above is still available.</p>
			{:else if error}
				<p class="error" role="alert">{error}</p>
			{:else if lines.length === 0}
				<p class="muted">The log file is empty.</p>
			{:else}
				<pre>{lines.join('\n')}</pre>
			{/if}
		</div>
	{/if}
</div>

<style>
	.log-viewer {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.toggle {
		align-self: flex-start;
		border: 1px solid #c3ccd7;
		background: #f5f7fa;
		border-radius: 0.35rem;
		padding: 0.25rem 0.7rem;
		font-size: 0.82rem;
		cursor: pointer;
	}

	.toggle:hover {
		background: #eaeff5;
	}

	.body pre {
		margin: 0;
		max-height: 20rem;
		overflow: auto;
		background: #10151c;
		color: #d7e0ea;
		padding: 0.6rem 0.75rem;
		border-radius: 0.4rem;
		font-size: 0.78rem;
		line-height: 1.45;
	}

	.muted {
		margin: 0;
		color: #6b7a8c;
		font-size: 0.85rem;
	}

	.error {
		margin: 0;
		color: #7f1d17;
		font-size: 0.85rem;
	}
</style>
