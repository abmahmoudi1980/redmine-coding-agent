<script lang="ts">
	import type { RepositoryOption } from '$lib/api';

	let {
		repository = null,
		candidates = [],
		selectedRepositoryId = null,
		busy = false,
		onselect = undefined
	}: {
		repository?: RepositoryOption | null;
		candidates?: RepositoryOption[];
		selectedRepositoryId?: number | null;
		busy?: boolean;
		onselect?: (repositoryId: number) => void;
	} = $props();

	const showsCard = $derived(repository !== null || candidates.length > 0);
</script>

{#if showsCard}
	<article class="card" data-testid="repository-card">
		<header class="card-head">
			<p class="eyebrow">Destination</p>
			{#if repository}
				<h2>{repository.displayName || repository.path}</h2>
			{:else}
				<h2>Repository selection required</h2>
			{/if}
		</header>

		{#if repository}
			<dl class="facts">
				<div>
					<dt>Repository</dt>
					<dd>{repository.path}</dd>
				</div>
				<div>
					<dt>Base branch</dt>
					<dd>{repository.baseBranch}</dd>
				</div>
				{#if repository.isDefault}
					<div>
						<dt>Mapping</dt>
						<dd>configured default</dd>
					</div>
				{/if}
			</dl>
			<p class="note">
				The worker will branch from <code>{repository.baseBranch}</code>. Nothing is written to this
				branch.
			</p>
		{:else}
			<p class="note">
				This project maps to more than one repository and no default is configured. Choose one to
				preview it again — the worker never guesses.
			</p>
			<ul class="candidates">
				{#each candidates as candidate (candidate.repositoryId)}
					<li>
						<div>
							<p class="candidate-path">{candidate.displayName || candidate.path}</p>
							<p class="candidate-meta">{candidate.path} · base {candidate.baseBranch}</p>
						</div>
						<button
							type="button"
							disabled={busy || selectedRepositoryId === candidate.repositoryId}
							onclick={() => onselect?.(candidate.repositoryId)}
						>
							Use this repository
						</button>
					</li>
				{/each}
			</ul>
		{/if}
	</article>
{/if}

<style>
	.card {
		background: #fff;
		border: 1px solid #dde2e8;
		border-radius: 0.5rem;
		padding: 1.25rem 1.5rem;
		display: flex;
		flex-direction: column;
		gap: 0.85rem;
	}

	.eyebrow {
		margin: 0;
		font-size: 0.8rem;
		letter-spacing: 0.04em;
		text-transform: uppercase;
		color: #5b6b7f;
	}

	h2 {
		margin: 0.15rem 0 0;
		font-size: 1.1rem;
	}

	.card-head {
		display: flex;
		flex-direction: column;
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

	.note {
		margin: 0;
		color: #4a5868;
		font-size: 0.88rem;
	}

	.candidates {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.candidates li {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
		border: 1px solid #e4e8ee;
		border-radius: 0.4rem;
		padding: 0.6rem 0.75rem;
	}

	.candidate-path {
		margin: 0;
		font-weight: 600;
	}

	.candidate-meta {
		margin: 0.15rem 0 0;
		font-size: 0.8rem;
		color: #6b7a8c;
	}

	button {
		border: 1px solid #10233f;
		background: #10233f;
		color: #fff;
		border-radius: 0.35rem;
		padding: 0.4rem 0.75rem;
		font: inherit;
		font-size: 0.85rem;
		cursor: pointer;
		white-space: nowrap;
	}

	button:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
