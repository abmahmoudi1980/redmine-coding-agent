<script lang="ts">
	import { tick } from 'svelte';
	import { ApiError, previewIssue, type IssuePreview, type Problem } from '$lib/api';
	import IssueCard from '$lib/components/IssueCard.svelte';
	import RepositoryPicker from '$lib/components/RepositoryPicker.svelte';

	let rawIssueId = $state('');
	let inputError = $state('');
	let preview = $state<IssuePreview | null>(null);
	let problem = $state<Problem | null>(null);
	let candidates = $state<IssuePreview['candidates']>([]);
	let selectedRepositoryId = $state<number | null>(null);
	let loading = $state(false);
	let inputEl = $state<HTMLInputElement | null>(null);

	// Starting a run is wired in User Story 2; until then the control is inert.
	const startImplemented = false;
	const confirmDisabled = $derived(!startImplemented || preview === null || loading);
	const showRepository = $derived(preview !== null || (candidates?.length ?? 0) > 0);

	function parseIssueId(value: string): number | null {
		const trimmed = value.trim();
		if (!/^[1-9][0-9]*$/.test(trimmed)) {
			inputError = 'Enter the issue number as a positive integer, for example 4821.';
			return null;
		}
		const parsed = Number(trimmed);
		if (!Number.isSafeInteger(parsed)) {
			inputError = 'That issue number is too large to be valid.';
			return null;
		}
		inputError = '';
		return parsed;
	}

	async function fetchPreview(repositoryId?: number) {
		const issueId = parseIssueId(rawIssueId);
		if (issueId === null) {
			return;
		}

		loading = true;
		problem = null;
		try {
			const result = await previewIssue(issueId, repositoryId ?? selectedRepositoryId ?? undefined);
			preview = result;
			candidates = result.candidates ?? [];
			selectedRepositoryId = result.repository.repositoryId;
		} catch (error) {
			preview = null;
			selectedRepositoryId = null;
			if (error instanceof ApiError) {
				problem = error.problem;
				candidates = error.problem.candidates ?? [];
			} else {
				candidates = [];
				problem = {
					type: 'about:blank',
					title: 'Unexpected error',
					status: 0,
					code: 'unexpected',
					detail: 'Something went wrong while previewing the issue. Try again.'
				};
			}
		} finally {
			loading = false;
		}
	}

	function onSubmit(event: SubmitEvent) {
		event.preventDefault();
		void fetchPreview();
	}

	function chooseRepository(repositoryId: number) {
		selectedRepositoryId = repositoryId;
		void fetchPreview(repositoryId);
	}

	async function decline() {
		rawIssueId = '';
		inputError = '';
		preview = null;
		problem = null;
		candidates = [];
		selectedRepositoryId = null;
		await tick();
		inputEl?.focus();
	}

	function warningText(code: string): string {
		switch (code) {
			case 'no_acceptance_criteria':
				return 'No acceptance-criteria heading was found. The agent will work from the description only.';
			case 'attachments_unavailable':
				return 'Some attachments could not be retrieved. The agent will not see them.';
			default:
				return code.replaceAll('_', ' ');
		}
	}
</script>

<section class="panel">
	<h1>Review a Redmine issue</h1>
	<p class="lede">
		Enter an issue number to see exactly what the worker found and where it would work. Nothing is
		created until you confirm.
	</p>

	<form onsubmit={onSubmit} novalidate>
		<label for="issue-number">Issue number</label>
		<div class="input-row">
			<input
				id="issue-number"
				bind:this={inputEl}
				bind:value={rawIssueId}
				inputmode="numeric"
				autocomplete="off"
				placeholder="4821"
				aria-invalid={inputError !== ''}
				aria-describedby={inputError !== '' ? 'issue-number-error' : undefined}
			/>
			<button type="submit" disabled={loading}>
				{loading ? 'Previewing…' : 'Preview issue'}
			</button>
		</div>
		{#if inputError}
			<p class="field-error" id="issue-number-error">{inputError}</p>
		{/if}
	</form>
</section>

{#if loading}
	<p class="status" role="status" aria-live="polite">Retrieving the issue and resolving its repository…</p>
{/if}

{#if problem}
	<div class="problem" role="alert" data-testid="problem">
		<h2>{problem.title}</h2>
		<p>{problem.detail}</p>
		{#if problem.stage}
			<p class="problem-meta">Stage: {problem.stage}</p>
		{/if}
	</div>
{/if}

{#if preview}
	{#if preview.warnings && preview.warnings.length > 0}
		<ul class="warnings">
			{#each preview.warnings as warning (warning)}
				<li>{warningText(warning)}</li>
			{/each}
		</ul>
	{/if}

	<IssueCard issue={preview.issue} />
{/if}

{#if showRepository}
	<RepositoryPicker
		repository={preview?.repository ?? null}
		candidates={candidates ?? []}
		selectedRepositoryId={selectedRepositoryId}
		busy={loading}
		onselect={chooseRepository}
	/>
{/if}

{#if preview || problem}
	<section class="controls">
		<button type="button" class="confirm" disabled={confirmDisabled} title="Enabled in the next milestone">
			Confirm and start
		</button>
		<button type="button" class="decline" onclick={decline}>Decline</button>
		<p class="controls-note">
			Starting a run arrives with the next milestone. Declining leaves no run, workspace, or branch.
		</p>
	</section>
{/if}

<style>
	.panel {
		background: #fff;
		border: 1px solid #dde2e8;
		border-radius: 0.5rem;
		padding: 1.25rem 1.5rem;
	}

	h1 {
		margin: 0 0 0.35rem;
		font-size: 1.5rem;
	}

	.lede {
		margin: 0 0 1rem;
		color: #4a5868;
		max-width: 52rem;
	}

	form {
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
		max-width: 28rem;
	}

	label {
		font-size: 0.8rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #4a5868;
	}

	.input-row {
		display: flex;
		gap: 0.5rem;
	}

	input {
		flex: 1 1 auto;
		border: 1px solid #c3ccd7;
		border-radius: 0.35rem;
		padding: 0.5rem 0.65rem;
		font: inherit;
	}

	input[aria-invalid='true'] {
		border-color: #b3261e;
	}

	.field-error {
		margin: 0;
		color: #b3261e;
		font-size: 0.85rem;
	}

	button {
		border-radius: 0.35rem;
		padding: 0.5rem 0.9rem;
		font: inherit;
		cursor: pointer;
	}

	form button {
		border: 1px solid #10233f;
		background: #10233f;
		color: #fff;
	}

	button:disabled {
		opacity: 0.45;
		cursor: not-allowed;
	}

	.status {
		margin: 0.75rem 0 0;
		color: #4a5868;
	}

	.problem {
		margin-top: 0.75rem;
		border: 1px solid #e7b7b3;
		background: #fdf1f0;
		border-radius: 0.5rem;
		padding: 1rem 1.25rem;
	}

	.problem h2 {
		margin: 0 0 0.35rem;
		font-size: 1rem;
		color: #7f1d17;
	}

	.problem p {
		margin: 0;
		color: #5d1a15;
	}

	.problem-meta {
		margin-top: 0.35rem !important;
		font-size: 0.8rem;
	}

	.warnings {
		margin: 0.75rem 0 0;
		padding: 0.6rem 1rem 0.6rem 2rem;
		background: #fdf7e8;
		border: 1px solid #ecd9a4;
		border-radius: 0.5rem;
		color: #6b4a06;
		font-size: 0.88rem;
	}

	section,
	ul,
	.controls {
		margin-top: 1rem;
	}

	.controls {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.75rem;
	}

	.controls .confirm {
		border: 1px solid #1e6b3a;
		background: #1e6b3a;
		color: #fff;
	}

	.controls .decline {
		border: 1px solid #c3ccd7;
		background: #fff;
		color: #1b1f24;
	}

	.controls-note {
		flex-basis: 100%;
		margin: 0;
		color: #6b7a8c;
		font-size: 0.82rem;
	}
</style>
