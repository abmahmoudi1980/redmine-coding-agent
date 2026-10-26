<script lang="ts">
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { ApiError, getRun, type Problem, type RunDetail } from '$lib/api';
	import CheckTable from '$lib/components/CheckTable.svelte';
	import ReportCard from '$lib/components/ReportCard.svelte';
	import Timeline from '$lib/components/Timeline.svelte';

	const runId = $derived(page.params.id ?? '');

	let run = $state<RunDetail | null>(null);
	let problem = $state<Problem | null>(null);
	let loading = $state(false);

	$effect(() => {
		const id = runId;
		if (!id) {
			return;
		}
		void load(id);
	});

	async function load(id: string) {
		loading = true;
		problem = null;
		try {
			run = await getRun(id);
		} catch (error) {
			run = null;
			problem =
				error instanceof ApiError
					? error.problem
					: {
							type: 'about:blank',
							title: 'Unexpected error',
							status: 0,
							code: 'unexpected',
							detail: 'The run could not be loaded. Try again.'
						};
		} finally {
			loading = false;
		}
	}

	function stateLabel(state?: string): string {
		switch (state) {
			case 'completed':
				return 'Completed';
			case 'failed':
				return 'Failed';
			case 'blocked':
				return 'Blocked';
			case 'cancelled':
				return 'Cancelled';
			case 'interrupted':
				return 'Interrupted';
			default:
				return state ? state.charAt(0).toUpperCase() + state.slice(1) : 'Queued';
		}
	}

	function elapsed(milliseconds?: number): string {
		if (!milliseconds || milliseconds <= 0) {
			return '—';
		}
		const seconds = Math.round(milliseconds / 1000);
		if (seconds < 60) {
			return `${seconds}s`;
		}
		const minutes = Math.floor(seconds / 60);
		return `${minutes}m ${seconds % 60}s`;
	}

	function shortRevision(value?: string): string {
		return value ? value.slice(0, 12) : '—';
	}
</script>

<svelte:head>
	<title>Run {runId} — RedmineCodingAgent</title>
</svelte:head>

<section class="panel">
	<nav class="breadcrumb">
		<a href={resolve('/')}>New run</a>
		<span aria-hidden="true">/</span>
		<span>Run {runId.slice(0, 8)}</span>
	</nav>

	{#if loading && !run}
		<p class="status" role="status">Loading the run…</p>
	{/if}

	{#if problem}
		<div class="problem" role="alert" data-testid="problem">
			<h2>{problem.title}</h2>
			<p>{problem.detail}</p>
		</div>
	{/if}

	{#if run}
		<header class="run-head">
			<div>
				<p class="eyebrow">Issue #{run.issueId}</p>
				<h1>{run.issueSubject || 'Run detail'}</h1>
			</div>
			<div class="state-block">
				<span class="state {run.state}" data-testid="run-state">{stateLabel(run.state)}</span>
				<span class="stage" data-testid="run-stage">{run.stage}</span>
			</div>
		</header>

		{#if run.failureCode}
			<div class="banner" data-testid="failure-banner">
				<h2>{run.failureCode.replaceAll('_', ' ')}</h2>
				<p>{run.failureMessage || 'The run did not finish successfully.'}</p>
			</div>
		{/if}

		<dl class="facts">
			<div>
				<dt>Repository</dt>
				<dd>{run.repository?.path ?? '—'}</dd>
			</div>
			<div>
				<dt>Branch</dt>
				<dd><code>{run.branch || '—'}</code></dd>
			</div>
			<div>
				<dt>Base</dt>
				<dd>{run.baseBranch || '—'} <span class="muted">@{shortRevision(run.baseRevision)}</span></dd>
			</div>
			<div>
				<dt>Commit</dt>
				<dd>{run.commitSha ? shortRevision(run.commitSha) : 'none'}</dd>
			</div>
			<div>
				<dt>Published</dt>
				<dd>
					{#if run.pushedAt}
						<span data-testid="pushed-at">branch pushed</span>
					{:else}
						<span class="muted">not pushed</span>
					{/if}
				</dd>
			</div>
			<div>
				<dt>Merge request</dt>
				<dd>
					{#if run.mergeRequestUrl}
						<a href={run.mergeRequestUrl} target="_blank" rel="noreferrer" data-testid="merge-request-link">
							review it
						</a>
					{:else}
						<span class="muted">not created</span>
					{/if}
				</dd>
			</div>
			<div>
				<dt>Elapsed</dt>
				<dd>{elapsed(run.elapsedMs)}</dd>
			</div>
			<div>
				<dt>Agent</dt>
				<dd>{run.agent?.name || '—'} <span class="muted">{run.agent?.model || ''}</span></dd>
			</div>
		</dl>

		<p class="assurance">
			Nothing is merged and the base branch is never written. Only a dedicated branch is pushed, and the
			merge request waits for a human to review and merge it.
		</p>

		<section class="block">
			<h2>Changed files</h2>
			{#if run.workspace?.changedPaths?.length}
				<ul class="files" data-testid="changed-files">
					{#each run.workspace.changedPaths as path (path)}
						<li><code>{path}</code></li>
					{/each}
				</ul>
			{:else}
				<p class="empty">No file changed.</p>
			{/if}
			{#if run.workspace?.hasUnpushedChanges}
				<p class="note">The workspace still holds unpushed work; cleanup is refused until it is published.</p>
			{/if}
		</section>

		<section class="block">
			<h2>Agent attempts</h2>
			{#if run.invocations?.length}
				<table class="invocations" data-testid="invocations">
					<thead>
						<tr>
							<th scope="col">#</th>
							<th scope="col">Kind</th>
							<th scope="col">Status</th>
							<th scope="col">Exit</th>
							<th scope="col">Duration</th>
							<th scope="col">Claimed</th>
						</tr>
					</thead>
					<tbody>
						{#each run.invocations as invocation (invocation.attemptNo)}
							<tr>
								<td>{invocation.attemptNo}</td>
								<td>{invocation.kind}</td>
								<td>{invocation.status}</td>
								<td>{invocation.exitCode ?? '—'}</td>
								<td>{elapsed(invocation.durationMs)}</td>
								<td class="muted">{invocation.reportedStatus || 'not reported'}</td>
							</tr>
						{/each}
					</tbody>
				</table>
			{:else}
				<p class="empty">The agent has not been invoked.</p>
			{/if}
		</section>

		<section class="block">
			<h2>Checks the worker ran</h2>
			<p class="note">
				These results come from the worker, not from the agent. A required check that did not pass
				blocks the commit.
			</p>
			<CheckTable checks={run.checks ?? []} />
		</section>

		<section class="block">
			<h2>Publish and report</h2>
			<p class="note">
				The branch was pushed only because every required check passed. The merge request targets
				<code>{run.baseBranch || 'the base branch'}</code> and is never merged by this tool.
			</p>
			<ReportCard
				report={run.report ?? null}
				commitSha={run.commitSha ?? ''}
				mergeRequestUrl={run.mergeRequestUrl ?? ''}
			/>
		</section>

		<section class="block">
			<h2>Timeline</h2>
			<Timeline events={run.timeline ?? []} />
		</section>
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

	.breadcrumb {
		display: flex;
		gap: 0.4rem;
		font-size: 0.82rem;
		color: #6b7a8c;
	}

	.breadcrumb a {
		color: #10233f;
	}

	.run-head {
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

	h2 {
		margin: 0 0 0.4rem;
		font-size: 1rem;
	}

	.state-block {
		display: flex;
		flex-direction: column;
		align-items: flex-end;
		gap: 0.2rem;
	}

	.state {
		border-radius: 999px;
		padding: 0.15rem 0.7rem;
		font-weight: 600;
		font-size: 0.85rem;
		border: 1px solid #c3ccd7;
		background: #f5f7fa;
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

	.stage {
		font-size: 0.78rem;
		color: #6b7a8c;
	}

	.facts {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem 1.75rem;
		margin: 0;
	}

	.facts div {
		display: flex;
		flex-direction: column;
		gap: 0.15rem;
	}

	dt {
		font-size: 0.72rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #6b7a8c;
	}

	dd {
		margin: 0;
		font-weight: 600;
	}

	.muted {
		color: #6b7a8c;
		font-weight: 500;
	}

	.assurance {
		margin: 0;
		padding: 0.55rem 0.75rem;
		border-radius: 0.4rem;
		background: #eef3f9;
		color: #274158;
		font-size: 0.85rem;
	}

	.banner {
		border: 1px solid #e7b7b3;
		background: #fdf1f0;
		border-radius: 0.5rem;
		padding: 0.8rem 1rem;
	}

	.banner h2 {
		color: #7f1d17;
	}

	.banner p {
		margin: 0;
		color: #5d1a15;
	}

	.block {
		border-top: 1px solid #e6eaf0;
		padding-top: 0.85rem;
	}

	.files {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.2rem;
		font-size: 0.85rem;
	}

	.invocations {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.88rem;
	}

	.invocations th,
	.invocations td {
		text-align: left;
		padding: 0.4rem 0.6rem;
		border-bottom: 1px solid #e6eaf0;
	}

	.invocations thead th {
		font-size: 0.72rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #6b7a8c;
	}

	.note {
		margin: 0 0 0.6rem;
		color: #4a5868;
		font-size: 0.85rem;
	}

	.empty {
		margin: 0;
		color: #6b7a8c;
		font-size: 0.88rem;
	}

	.status {
		margin: 0;
		color: #4a5868;
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
	}

	.problem p {
		margin: 0;
		color: #5d1a15;
	}
</style>
