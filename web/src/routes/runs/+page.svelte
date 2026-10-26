<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { ApiError, listRuns, retryRun, type Problem, type Run } from '$lib/api';

	const stateOptions = [
		'',
		'queued',
		'preparing',
		'implementing',
		'validating',
		'repairing',
		'publishing',
		'reporting',
		'completed',
		'failed',
		'blocked',
		'cancelled',
		'interrupted'
	];

	let items = $state<Run[]>([]);
	let nextCursor = $state<string | null>(null);
	let stateFilter = $state('');
	let issueFilter = $state('');
	let appliedIssue = $state<number | null>(null);
	let loading = $state(false);
	let loadingMore = $state(false);
	let problem = $state<Problem | null>(null);
	let retrying = $state<string | null>(null);
	let retryProblem = $state<Problem | null>(null);

	onMount(() => {
		void load(true);
	});

	async function load(reset: boolean) {
		if (reset) {
			loading = true;
			problem = null;
			nextCursor = null;
		} else {
			loadingMore = true;
		}
		try {
			const page = await listRuns({
				state: stateFilter || undefined,
				issueId: appliedIssue ?? undefined,
				limit: 25,
				cursor: reset ? undefined : nextCursor ?? undefined
			});
			items = reset ? (page.items ?? []) : [...items, ...(page.items ?? [])];
			nextCursor = page.nextCursor ?? null;
		} catch (error) {
			problem = asProblem(error, 'The run history could not be loaded.');
		} finally {
			loading = false;
			loadingMore = false;
		}
	}

	function applyFilters(event: SubmitEvent) {
		event.preventDefault();
		const trimmed = issueFilter.trim();
		appliedIssue = trimmed === '' ? null : Number(trimmed);
		if (appliedIssue !== null && (!Number.isSafeInteger(appliedIssue) || appliedIssue < 1)) {
			problem = {
				type: 'about:blank',
				title: 'Invalid issue number',
				status: 0,
				code: 'invalid_issue_id',
				detail: 'Enter the issue number as a positive integer, or leave it empty.'
			};
			return;
		}
		items = [];
		void load(true);
	}

	async function retry(run: Run) {
		retrying = run.runId;
		retryProblem = null;
		try {
			const created = await retryRun(run.runId);
			await goto(resolve('/runs/[id]', { id: created.runId }));
		} catch (error) {
			retryProblem = asProblem(error, 'The retry could not be started.');
			retrying = null;
		}
	}

	function canRetry(run: Run): boolean {
		return run.terminal === true && ['failed', 'blocked', 'interrupted', 'cancelled'].includes(run.state);
	}

	function asProblem(error: unknown, fallback: string): Problem {
		if (error instanceof ApiError) {
			return error.problem;
		}
		return {
			type: 'about:blank',
			title: 'Unexpected error',
			status: 0,
			code: 'unexpected',
			detail: fallback
		};
	}

	function stateLabel(state: string): string {
		return state ? state.charAt(0).toUpperCase() + state.slice(1) : '—';
	}

	function time(value?: string | null): string {
		if (!value) {
			return '—';
		}
		const parsed = new Date(value);
		if (Number.isNaN(parsed.getTime())) {
			return value;
		}
		return parsed.toLocaleString();
	}
</script>

<svelte:head>
	<title>Run history — RedmineCodingAgent</title>
</svelte:head>

<section class="panel">
	<header class="head">
		<div>
			<p class="eyebrow">Audit trail</p>
			<h1>Run history</h1>
		</div>
		<a class="primary" href={resolve('/')}>New run</a>
	</header>

	<p class="note">
		Every attempt survives a restart. A retry is a new, linked attempt and never resumes or
		force-pushes the previous one.
	</p>

	<form class="filters" onsubmit={applyFilters}>
		<label>
			State
			<select bind:value={stateFilter}>
				{#each stateOptions as option (option)}
					<option value={option}>{option === '' ? 'Any state' : stateLabel(option)}</option>
				{/each}
			</select>
		</label>
		<label>
			Issue
			<input type="text" inputmode="numeric" placeholder="4821" bind:value={issueFilter} />
		</label>
		<button type="submit" class="primary">Apply</button>
	</form>

	{#if problem}
		<div class="problem" role="alert" data-testid="problem">
			<h2>{problem.title}</h2>
			<p>{problem.detail}</p>
		</div>
	{/if}

	{#if retryProblem}
		<div class="problem" role="alert" data-testid="retry-problem">
			<h2>{retryProblem.title}</h2>
			<p>{retryProblem.detail}</p>
		</div>
	{/if}

	{#if loading}
		<p class="status" role="status">Loading the history…</p>
	{:else if items.length === 0}
		<p class="empty" data-testid="empty-history">No runs match this filter yet.</p>
	{:else}
		<table class="history" data-testid="run-history">
			<thead>
				<tr>
					<th scope="col">Run</th>
					<th scope="col">Issue</th>
					<th scope="col">Attempt</th>
					<th scope="col">State</th>
					<th scope="col">Stage</th>
					<th scope="col">Created</th>
					<th scope="col"></th>
				</tr>
			</thead>
			<tbody>
				{#each items as run (run.runId)}
					<tr>
						<td>
							<a href={resolve('/runs/[id]', { id: run.runId })}>
								<code>{run.runId.slice(0, 8)}</code>
							</a>
						</td>
						<td>
							<span class="issue">#{run.issueId}</span>
							<span class="subject">{run.issueSubject ?? ''}</span>
						</td>
						<td>{run.attemptNo ?? 1}</td>
						<td>
							<span class="state {run.state}" data-testid="state-badge">{stateLabel(run.state)}</span>
						</td>
						<td><code>{run.stage}</code></td>
						<td>{time(run.createdAt)}</td>
						<td class="actions">
							{#if canRetry(run)}
								<button
									type="button"
									class="ghost"
									disabled={retrying === run.runId}
									onclick={() => retry(run)}
								>
									{retrying === run.runId ? 'Retrying…' : 'Retry'}
								</button>
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>

		{#if nextCursor}
			<button
				type="button"
				class="ghost more"
				disabled={loadingMore}
				onclick={() => load(false)}
			>
				{loadingMore ? 'Loading…' : 'Load more'}
			</button>
		{/if}
	{/if}
</section>

<style>
	.panel {
		background: #fff;
		border: 1px solid #dde2e8;
		border-radius: 0.5rem;
		padding: 1.25rem 1.5rem;
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.head {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		gap: 1rem;
		flex-wrap: wrap;
	}

	.eyebrow {
		margin: 0;
		font-size: 0.78rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #5b6b7f;
	}

	h1 {
		margin: 0.2rem 0 0;
		font-size: 1.35rem;
	}

	.note {
		margin: 0;
		color: #4a5868;
		font-size: 0.88rem;
	}

	.filters {
		display: flex;
		gap: 0.85rem;
		align-items: flex-end;
		flex-wrap: wrap;
	}

	label {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
		font-size: 0.78rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #6b7a8c;
	}

	select,
	input {
		font: inherit;
		text-transform: none;
		letter-spacing: normal;
		padding: 0.35rem 0.5rem;
		border: 1px solid #c3ccd7;
		border-radius: 0.35rem;
		background: #fff;
		color: #1b1f24;
		min-width: 9rem;
	}

	button {
		font: inherit;
		cursor: pointer;
		border-radius: 0.4rem;
		padding: 0.4rem 0.9rem;
		border: 1px solid #c3ccd7;
		background: #f5f7fa;
	}

	button.primary,
	a.primary {
		background: #10233f;
		border-color: #10233f;
		color: #fff;
		text-decoration: none;
		padding: 0.4rem 0.9rem;
		border-radius: 0.4rem;
		font-size: 0.9rem;
	}

	button.ghost {
		background: #fff;
	}

	button:disabled {
		opacity: 0.6;
		cursor: default;
	}

	.history {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.88rem;
	}

	.history th,
	.history td {
		text-align: left;
		padding: 0.45rem 0.6rem;
		border-bottom: 1px solid #e6eaf0;
		vertical-align: top;
	}

	.history thead th {
		font-size: 0.72rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #6b7a8c;
	}

	.issue {
		font-weight: 600;
		margin-right: 0.35rem;
	}

	.subject {
		color: #4a5868;
	}

	.state {
		border-radius: 999px;
		padding: 0.12rem 0.6rem;
		font-weight: 600;
		font-size: 0.78rem;
		border: 1px solid #c3ccd7;
		background: #f5f7fa;
		white-space: nowrap;
	}

	.state.completed {
		background: #edf7ef;
		border-color: #a9d4b4;
		color: #1e6b3a;
	}

	.state.failed,
	.state.blocked {
		background: #fdf1f0;
		border-color: #e7b7b3;
		color: #7f1d17;
	}

	.state.cancelled,
	.state.interrupted {
		background: #f5f3ee;
		border-color: #d9d2c2;
		color: #6b5a2b;
	}

	.actions {
		text-align: right;
	}

	.more {
		align-self: center;
	}

	.problem {
		border: 1px solid #e7b7b3;
		background: #fdf1f0;
		border-radius: 0.5rem;
		padding: 0.8rem 1rem;
	}

	.problem h2 {
		margin: 0 0 0.35rem;
		color: #7f1d17;
		font-size: 1rem;
	}

	.problem p {
		margin: 0;
		color: #5d1a15;
	}

	.empty,
	.status {
		margin: 0;
		color: #6b7a8c;
		font-size: 0.88rem;
	}
</style>
