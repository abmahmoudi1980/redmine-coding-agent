<script lang="ts">
	import { onMount } from 'svelte';
	import { ApiError, getHealth, type HealthReport, type Problem } from '$lib/api';

	let report = $state<HealthReport | null>(null);
	let deep = $state(false);
	let loading = $state(false);
	let problem = $state<Problem | null>(null);

	onMount(() => {
		void load();
	});

	async function load() {
		loading = true;
		problem = null;
		try {
			report = await getHealth(deep);
		} catch (error) {
			report = null;
			problem = asProblem(error);
		} finally {
			loading = false;
		}
	}

	function asProblem(error: unknown): Problem {
		if (error instanceof ApiError) {
			return error.problem;
		}
		return {
			type: 'about:blank',
			title: 'Unexpected error',
			status: 0,
			code: 'unexpected',
			detail: 'The health report could not be loaded.'
		};
	}

	function statusLabel(status: string): string {
		return status === 'ok' ? 'Healthy' : status === 'failing' ? 'Failing' : 'Unknown';
	}

	function latency(component: HealthReport['components'][number]): string {
		return typeof component.latencyMs === 'number' ? `${component.latencyMs} ms` : '—';
	}

	function checkedAt(value?: string): string {
		if (!value) {
			return '—';
		}
		const parsed = new Date(value);
		return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
	}
</script>

<svelte:head>
	<title>Health — RedmineCodingAgent</title>
</svelte:head>

<section class="panel">
	<header class="panel-head">
		<div>
			<h1>Health</h1>
			<p class="hint">
				Each dependency is probed on its own; one failing component degrades the report without
				failing the request.
			</p>
		</div>
		<div class="controls">
			<label>
				<input type="checkbox" bind:checked={deep} onchange={() => void load()} />
				Deep probe
			</label>
			<button type="button" onclick={() => void load()} disabled={loading}>
				{loading ? 'Checking…' : 'Refresh'}
			</button>
		</div>
	</header>

	{#if problem}
		<div class="problem" role="alert" data-testid="problem">{problem.detail}</div>
	{/if}

	{#if report}
		<p class="summary" data-testid="health-summary">
			<span class:degraded={report.status === 'degraded'} class:ok={report.status === 'ok'}></span>
			Overall: {report.status === 'degraded' ? 'Degraded' : 'OK'} · checked {checkedAt(
				report.checkedAt
			)}
		</p>

		<ul class="components">
			{#each report.components as component (component.name)}
				<li class="component" data-testid="health-component" data-name={component.name}>
					<div class="row">
						<strong>{component.name}</strong>
						<span
							class="status"
							class:ok={component.status === 'ok'}
							class:failing={component.status === 'failing'}
							class:unknown={component.status === 'unknown'}
							data-testid="health-status"
						>
							{statusLabel(component.status)}
						</span>
						<span class="latency" data-testid="health-latency">{latency(component)}</span>
					</div>
					{#if component.detail}
						<p class="detail">{component.detail}</p>
					{/if}
					{#if component.remediation}
						<p class="remediation" data-testid="health-remediation">
							Fix: {component.remediation}
						</p>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}
</section>

<style>
	.panel {
		background: #fff;
		border: 1px solid #dde3ea;
		border-radius: 0.75rem;
		padding: 1.5rem;
	}

	.panel-head {
		display: flex;
		gap: 1rem;
		align-items: flex-start;
		justify-content: space-between;
	}

	h1 {
		margin: 0 0 0.25rem;
		font-size: 1.35rem;
	}

	.hint {
		margin: 0;
		color: #55606d;
		font-size: 0.9rem;
		max-width: 46rem;
	}

	.controls {
		display: flex;
		gap: 0.75rem;
		align-items: center;
		white-space: nowrap;
	}

	.controls label {
		font-size: 0.85rem;
		color: #35414f;
	}

	.controls button {
		padding: 0.45rem 0.9rem;
		border-radius: 0.4rem;
		border: 1px solid #10233f;
		background: #10233f;
		color: #fff;
		cursor: pointer;
	}

	.controls button:disabled {
		opacity: 0.6;
		cursor: default;
	}

	.summary {
		display: flex;
		align-items: center;
		gap: 0.4rem;
		margin: 1rem 0;
		font-size: 0.9rem;
		color: #35414f;
	}

	.summary span {
		width: 0.55rem;
		height: 0.55rem;
		border-radius: 50%;
		background: #8b98a8;
	}

	.summary span.ok {
		background: #1f9d55;
	}

	.summary span.degraded {
		background: #d97706;
	}

	.problem {
		margin: 1rem 0;
		padding: 0.75rem 1rem;
		border-radius: 0.5rem;
		background: #fdecea;
		border: 1px solid #f3b4ab;
		color: #8b1e12;
	}

	.components {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: 0.75rem;
	}

	.component {
		border: 1px solid #e3e8ee;
		border-radius: 0.6rem;
		padding: 0.9rem 1rem;
		background: #fbfcfd;
	}

	.row {
		display: flex;
		align-items: center;
		gap: 0.75rem;
	}

	.row strong {
		min-width: 7rem;
		text-transform: capitalize;
	}

	.status {
		padding: 0.1rem 0.5rem;
		border-radius: 999px;
		font-size: 0.75rem;
		background: #e2e8f0;
		color: #334155;
	}

	.status.ok {
		background: #e3f5ea;
		color: #166534;
	}

	.status.failing {
		background: #fdecea;
		color: #8b1e12;
	}

	.status.unknown {
		background: #fef3c7;
		color: #92400e;
	}

	.latency {
		margin-left: auto;
		font-variant-numeric: tabular-nums;
		color: #55606d;
		font-size: 0.85rem;
	}

	.detail {
		margin: 0.45rem 0 0;
		font-size: 0.88rem;
		color: #35414f;
		word-break: break-word;
	}

	.remediation {
		margin: 0.3rem 0 0;
		font-size: 0.85rem;
		color: #92400e;
	}
</style>
