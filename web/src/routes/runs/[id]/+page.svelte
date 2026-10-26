<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import {
		ApiError,
		cancelRun,
		getRun,
		retryRun,
		type Problem,
		type Run,
		type RunDetail
	} from '$lib/api';
	import CheckTable from '$lib/components/CheckTable.svelte';
	import LogViewer from '$lib/components/LogViewer.svelte';
	import ReportCard from '$lib/components/ReportCard.svelte';
	import Timeline from '$lib/components/Timeline.svelte';
	import { isTerminalState, streamRunEvents, type SseEvent, type StreamStatus } from '$lib/sse';

	const runId = $derived(page.params.id ?? '');

	const steps = [
		{ label: 'Preparing', stage: 'preparing' },
		{ label: 'Implementing', stage: 'implementing' },
		{ label: 'Validating', stage: 'validating' },
		{ label: 'Publishing', stage: 'publishing' },
		{ label: 'Reporting', stage: 'reporting' }
	];

	type ActivityLine = { seq: number; at: string; message: string; severity: string };

	let run = $state<RunDetail | null>(null);
	let problem = $state<Problem | null>(null);
	let loading = $state(false);
	let activity = $state<ActivityLine[]>([]);
	let streamStatus = $state<StreamStatus>('closed');
	let confirmCancel = $state(false);
	let cancelling = $state(false);
	let cancelProblem = $state<Problem | null>(null);
	let retrying = $state(false);
	let retryProblem = $state<Problem | null>(null);
	let now = $state(Date.now());

	let teardown: (() => void) | null = null;
	let refreshTimer: ReturnType<typeof setTimeout> | null = null;

	const currentStep = $derived(
		Math.max(stepIndexFromState(run?.state), stepIndexFromStage(run?.stage))
	);

	const elapsedLabel = $derived.by(() => {
		if (!run) {
			return '—';
		}
		const start = Date.parse(run.startedAt ?? run.createdAt ?? '');
		if (Number.isNaN(start)) {
			return '—';
		}
		const finish = run.endedAt ? Date.parse(run.endedAt) : now;
		return durationLabel(Math.max(0, finish - start));
	});

	const summary = $derived(run ? describeOutcome(run) : null);

	$effect(() => {
		const id = runId;
		if (!id) {
			return;
		}
		void load(id);
		return () => {
			stopStream();
			if (refreshTimer) {
				clearTimeout(refreshTimer);
				refreshTimer = null;
			}
		};
	});

	$effect(() => {
		const interval = setInterval(() => {
			now = Date.now();
		}, 1000);
		return () => clearInterval(interval);
	});

	async function load(id: string) {
		loading = true;
		problem = null;
		try {
			const detail = await getRun(id);
			if (id !== runId) {
				return;
			}
			applyRun(detail);
			activity = (detail.timeline ?? [])
				.filter((event) => event.type === 'activity')
				.map((event) => ({
					seq: event.seq,
					at: event.at,
					message: event.message,
					severity: event.severity
				}));
			if (!isTerminalState(detail.state)) {
				startStream(id);
			}
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

	function startStream(id: string) {
		stopStream();
		teardown = streamRunEvents(
			id,
			{
				onStatus: (status) => {
					streamStatus = status;
				},
				onEvent: (event) => handleEvent(id, event),
				onTerminal: (state) => {
					if (run) {
						run.state = state as RunDetail['state'];
					}
					scheduleRefresh(id);
				}
			},
			{ maxAttempts: 5 }
		);
	}

	function stopStream() {
		if (teardown) {
			teardown();
			teardown = null;
		}
		streamStatus = 'closed';
	}

	function handleEvent(id: string, event: SseEvent) {
		const data = event.data;

		if (event.type === 'activity') {
			const message = typeof data.message === 'string' ? data.message : '';
			if (message && !activity.some((line) => line.seq === event.id)) {
				activity = [
					...activity,
					{
						seq: event.id,
						at: data.at ?? new Date().toISOString(),
						message,
						severity: data.severity ?? 'info'
					}
				].slice(-200);
			}
			return;
		}

		if (event.type === 'state' && typeof data.state === 'string') {
			if (run) {
				run.state = data.state as RunDetail['state'];
				if (typeof data.stage === 'string') {
					run.stage = data.stage;
				}
			}
			scheduleRefresh(id);
			return;
		}

		if (event.type === 'stage' && typeof data.stage === 'string') {
			if (run) {
				run.stage = data.stage;
			}
			return;
		}

		if (['check', 'publish', 'report', 'workspace', 'agent'].includes(event.type)) {
			scheduleRefresh(id);
		}
	}

	function scheduleRefresh(id: string) {
		if (refreshTimer) {
			clearTimeout(refreshTimer);
		}
		refreshTimer = setTimeout(() => {
			refreshTimer = null;
			void refresh(id);
		}, 300);
	}

	async function refresh(id: string) {
		if (id !== runId) {
			return;
		}
		try {
			applyRun(await getRun(id));
		} catch {
			// Keep the last good view; the stream remains the source of progress.
		}
	}

	function applyRun(latest: RunDetail) {
		// A refresh can race the terminal transition. Never let a stale read pull
		// the view back out of a terminal state.
		if (run && isTerminalState(run.state) && !isTerminalState(latest.state)) {
			return;
		}
		run = latest;
		if (isTerminalState(latest.state)) {
			stopStream();
		}
	}

	async function confirmCancellation(id: string) {
		cancelling = true;
		cancelProblem = null;
		try {
			const updated = await cancelRun(id);
			run = mergeRun(run, updated);
			confirmCancel = false;
			stopStream();
			scheduleRefresh(id);
		} catch (error) {
			cancelProblem =
				error instanceof ApiError
					? error.problem
					: {
							type: 'about:blank',
							title: 'Cancellation failed',
							status: 0,
							code: 'unexpected',
							detail: 'The worker did not accept the cancellation.'
						};
		} finally {
			cancelling = false;
		}
	}

	async function retry(id: string) {
		retrying = true;
		retryProblem = null;
		try {
			const created = await retryRun(id);
			await goto(resolve('/runs/[id]', { id: created.runId }));
		} catch (error) {
			retryProblem =
				error instanceof ApiError
					? error.problem
					: {
							type: 'about:blank',
							title: 'Retry failed',
							status: 0,
							code: 'unexpected',
							detail: 'The worker did not start a new attempt.'
						};
			retrying = false;
		}
	}

	function canRetry(detail: RunDetail): boolean {
		return (
			isTerminalState(detail.state) &&
			['failed', 'blocked', 'interrupted', 'cancelled'].includes(detail.state)
		);
	}

	function mergeRun(current: RunDetail | null, update: Run): RunDetail | null {
		if (!current) {
			return current;
		}
		return { ...current, ...update } as RunDetail;
	}

	function stepIndexFromState(state?: string): number {
		switch (state) {
			case 'queued':
			case 'preparing':
				return 0;
			case 'implementing':
				return 1;
			case 'validating':
			case 'repairing':
				return 2;
			case 'publishing':
				return 3;
			case 'reporting':
			case 'completed':
				return 4;
			default:
				return -1;
		}
	}

	function stepIndexFromStage(stage?: string): number {
		if (!stage) {
			return -1;
		}
		if (stage.startsWith('checks.')) {
			return 2;
		}
		if (stage.startsWith('commit.') || stage.startsWith('branch.')) {
			return 3;
		}
		if (stage.startsWith('mr.') || stage.startsWith('comment.') || stage.startsWith('report.')) {
			return 4;
		}
		if (stage.startsWith('agent.') || stage.startsWith('workspace.')) {
			return 1;
		}
		if (stage.startsWith('connectivity.')) {
			return 0;
		}
		return -1;
	}

	function describeOutcome(detail: RunDetail): { headline: string; next: string } {
		const failure = detail.failureMessage ?? '';
		switch (detail.state) {
			case 'completed':
				return {
					headline: detail.commitSha
						? 'Every required check passed and the work was committed.'
						: 'The run completed without a commit.',
					next: detail.mergeRequestUrl
						? 'Review the merge request; a human decides whether it is merged.'
						: 'Open the changed files below. The branch was not published.'
				};
			case 'cancelled':
				return {
					headline: 'This run was cancelled. Nothing further was published.',
					next:
						'The workspace keeps any unpushed work, so start a new run only when you are ready. Completed side effects are never rolled back.'
				};
			case 'blocked':
				return {
					headline: failure || 'The agent stopped because the issue needs an answer.',
					next: 'Answer the blocker on the issue, then start a new run.'
				};
			case 'interrupted':
				return {
					headline: 'The worker stopped while this run was active.',
					next: 'Review the workspace, then retry explicitly. An interrupted run never resumes into a push.'
				};
			case 'failed':
				return {
					headline: failure || 'The run failed.',
					next:
						'Open the check results, the agent attempts, and the log tail below; then fix the cause and start a new run.'
				};
			default:
				return {
					headline: 'This run is still in progress.',
					next: 'You can leave this page open: the timeline updates as the worker advances.'
				};
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

	function durationLabel(milliseconds: number): string {
		const seconds = Math.round(milliseconds / 1000);
		if (seconds < 60) {
			return `${seconds}s`;
		}
		const minutes = Math.floor(seconds / 60);
		return `${minutes}m ${seconds % 60}s`;
	}

	function elapsed(milliseconds?: number): string {
		if (!milliseconds || milliseconds <= 0) {
			return '—';
		}
		return durationLabel(milliseconds);
	}

	function shortRevision(value?: string): string {
		return value ? value.slice(0, 12) : '—';
	}

	function time(value: string): string {
		const parsed = new Date(value);
		if (Number.isNaN(parsed.getTime())) {
			return value;
		}
		return parsed.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
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
				<span class="stream" data-testid="stream-status">
					{#if isTerminalState(run.state)}
						finished
					{:else if streamStatus === 'open'}
						live
					{:else if streamStatus === 'reconnecting'}
						reconnecting…
					{:else}
						connecting…
					{/if}
				</span>
			</div>
		</header>

		<section class="live-bar">
			<ol class="stepper" data-testid="stage-stepper">
				{#each steps as step, index (step.stage)}
					<li
						class:done={index < currentStep}
						class:current={index === currentStep}
						aria-current={index === currentStep ? 'step' : undefined}
					>
						{step.label}
					</li>
				{/each}
			</ol>
			<div class="timer">
				<span class="timer-label">Elapsed</span>
				<span class="timer-value" data-testid="elapsed-timer">{elapsedLabel}</span>
			</div>
		</section>

		{#if !isTerminalState(run.state)}
			<div class="cancel-box">
				{#if !confirmCancel}
					<button
						type="button"
						class="danger"
						data-testid="cancel-button"
						onclick={() => (confirmCancel = true)}
					>
						Cancel run
					</button>
				{:else}
					<div class="confirm" role="alertdialog" aria-label="Confirm cancellation">
						<p>
							Cancelling stops the agent and its child processes. Anything already completed stays
							recorded, and nothing is pushed after you confirm.
						</p>
						<div class="confirm-actions">
							<button
								type="button"
								class="danger"
								data-testid="cancel-confirm"
								disabled={cancelling}
								onclick={() => confirmCancellation(runId)}
							>
								{cancelling ? 'Cancelling…' : 'Yes, cancel the run'}
							</button>
							<button type="button" class="ghost" onclick={() => (confirmCancel = false)}>
								Keep running
							</button>
						</div>
					</div>
				{/if}
				{#if cancelProblem}
					<p class="cancel-error" role="alert" data-testid="cancel-problem">
						{cancelProblem.title}: {cancelProblem.detail}
					</p>
				{/if}
			</div>
		{/if}

		{#if run.previousRunId || (run.attemptNo ?? 1) > 1}
			<div class="lineage" data-testid="retry-lineage">
				<h2>Attempt {run.attemptNo ?? 2}</h2>
				<p>
					This run is a retry.
					{#if run.previousRunId}
						It is linked to
						<a href={resolve('/runs/[id]', { id: run.previousRunId })}>
							attempt {Math.max(1, (run.attemptNo ?? 2) - 1)}
						</a>, which is preserved exactly as it ended.
					{/if}
					The earlier attempt was never resumed and its branch was never force-pushed.
				</p>
			</div>
		{/if}

		{#if run.failureCode}
			<div class="banner" data-testid="failure-banner">
				<h2>{run.failureCode.replaceAll('_', ' ')}</h2>
				<p>{run.failureMessage || 'The run did not finish successfully.'}</p>
				{#if run.failureStage}
					<p class="meta" data-testid="failure-stage">
						Stopped at stage <code>{run.failureStage}</code>.
					</p>
				{/if}
				{#if run.failureRemediation}
					<p class="remediation" data-testid="failure-remediation">
						What to do next: {run.failureRemediation}
					</p>
				{/if}
			</div>
		{/if}

		{#if canRetry(run)}
			<div class="retry-box">
				<button
					type="button"
					class="primary"
					data-testid="retry-button"
					disabled={retrying}
					onclick={() => retry(runId)}
				>
					{retrying ? 'Starting a new attempt…' : 'Retry this issue'}
				</button>
				{#if retryProblem}
					<p class="cancel-error" role="alert" data-testid="retry-error">
						{retryProblem.title}: {retryProblem.detail}
					</p>
				{/if}
			</div>
		{/if}

		{#if summary}
			<section class="summary" data-testid="run-summary">
				<div>
					<h2>What happened</h2>
					<p>{summary.headline}</p>
				</div>
				<div>
					<h2>What to do next</h2>
					<p>{summary.next}</p>
				</div>
			</section>
		{/if}

		<section class="block">
			<h2>Activity</h2>
			{#if activity.length === 0}
				<p class="empty">No agent activity yet.</p>
			{:else}
				<ol class="activity" data-testid="activity-feed">
					{#each activity as line (line.seq)}
						<li class={line.severity}>
							<span class="at">{time(line.at)}</span>
							<span class="message">{line.message}</span>
						</li>
					{/each}
				</ol>
			{/if}
		</section>

		<dl class="facts">
			<div>
				<dt>Repository</dt>
				<dd>{run.repository?.path ?? '—'}</dd>
			</div>
			<div>
				<dt>Attempt</dt>
				<dd data-testid="attempt-number">{run.attemptNo ?? 1}</dd>
			</div>
			<div>
				<dt>Previous attempt</dt>
				<dd>
					{#if run.previousRunId}
						<a href={resolve('/runs/[id]', { id: run.previousRunId })}>
							<code>{run.previousRunId.slice(0, 8)}</code>
						</a>
					{:else}
						<span class="muted">none</span>
					{/if}
				</dd>
			</div>
			{#if run.interruptedStage}
				<div>
					<dt>Interrupted at</dt>
					<dd><code>{run.interruptedStage}</code></dd>
				</div>
			{/if}
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
			<h2>Logs</h2>
			<LogViewer runId={runId} />
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

	.stream {
		font-size: 0.72rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #6b7a8c;
	}

	.live-bar {
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 1rem;
		flex-wrap: wrap;
		border: 1px solid #e6eaf0;
		border-radius: 0.5rem;
		padding: 0.6rem 0.85rem;
		background: #fbfcfd;
	}

	.stepper {
		display: flex;
		gap: 0.4rem;
		list-style: none;
		margin: 0;
		padding: 0;
		flex-wrap: wrap;
	}

	.stepper li {
		border-radius: 999px;
		padding: 0.15rem 0.65rem;
		font-size: 0.78rem;
		border: 1px solid #d5dce4;
		color: #6b7a8c;
		background: #fff;
	}

	.stepper li.done {
		border-color: #a9d4b4;
		background: #edf7ef;
		color: #1e6b3a;
	}

	.stepper li.current {
		border-color: #10233f;
		background: #10233f;
		color: #fff;
		font-weight: 600;
	}

	.timer {
		display: flex;
		flex-direction: column;
		align-items: flex-end;
	}

	.timer-label {
		font-size: 0.7rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #6b7a8c;
	}

	.timer-value {
		font-size: 1.1rem;
		font-weight: 600;
		font-variant-numeric: tabular-nums;
	}

	.cancel-box {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	button {
		font: inherit;
		cursor: pointer;
		border-radius: 0.4rem;
		padding: 0.4rem 0.9rem;
		border: 1px solid #c3ccd7;
		background: #f5f7fa;
	}

	button.danger {
		border-color: #e7b7b3;
		background: #fdf1f0;
		color: #7f1d17;
		font-weight: 600;
		align-self: flex-start;
	}

	button.ghost {
		background: #fff;
	}

	.confirm {
		border: 1px solid #e7b7b3;
		background: #fdf1f0;
		border-radius: 0.5rem;
		padding: 0.7rem 0.9rem;
		display: flex;
		flex-direction: column;
		gap: 0.6rem;
	}

	.confirm p {
		margin: 0;
		color: #5d1a15;
		font-size: 0.86rem;
	}

	.confirm-actions {
		display: flex;
		gap: 0.5rem;
		flex-wrap: wrap;
	}

	.cancel-error {
		margin: 0;
		color: #7f1d17;
		font-size: 0.86rem;
	}

	.summary {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(16rem, 1fr));
		gap: 0.9rem;
		padding: 0.75rem 0.9rem;
		border: 1px solid #dbe2ea;
		border-radius: 0.5rem;
		background: #f7f9fb;
	}

	.summary p {
		margin: 0;
		color: #33475c;
		font-size: 0.88rem;
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

	.banner .meta {
		margin-top: 0.35rem;
		font-size: 0.85rem;
		color: #6b3a34;
	}

	.banner .remediation {
		margin-top: 0.45rem;
		padding: 0.45rem 0.6rem;
		border-radius: 0.35rem;
		background: #fff;
		color: #33475c;
		font-size: 0.88rem;
	}

	.lineage {
		border: 1px solid #cfd9e6;
		background: #f3f7fc;
		border-radius: 0.5rem;
		padding: 0.75rem 1rem;
	}

	.lineage h2 {
		color: #10233f;
	}

	.lineage p {
		margin: 0;
		color: #33475c;
		font-size: 0.88rem;
	}

	.retry-box {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	button.primary {
		background: #10233f;
		border-color: #10233f;
		color: #fff;
		align-self: flex-start;
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

	.activity {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.2rem;
		max-height: 16rem;
		overflow: auto;
		font-size: 0.85rem;
	}

	.activity li {
		display: grid;
		grid-template-columns: 5.5rem 1fr;
		gap: 0.6rem;
		padding: 0.25rem 0.4rem;
		border-left: 3px solid #dbe2ea;
		background: #fbfcfd;
	}

	.activity li.warn {
		border-left-color: #d9a441;
		background: #fdf9ee;
	}

	.activity li.error {
		border-left-color: #b3261e;
		background: #fdf1f0;
	}

	.activity .at {
		color: #6b7a8c;
		font-variant-numeric: tabular-nums;
	}

	.activity .message {
		color: #1b1f24;
		word-break: break-word;
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
