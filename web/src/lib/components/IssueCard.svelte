<script lang="ts">
	import type { IssueSnapshot } from '$lib/api';

	let { issue }: { issue: IssueSnapshot } = $props();

	const hasCriteria = $derived(
		issue.criteriaSource === 'heading' && (issue.acceptanceCriteria ?? '').trim() !== ''
	);
</script>

<article class="card" data-testid="issue-card">
	<header class="card-head">
		<p class="eyebrow">Issue #{issue.issueId}</p>
		<h2>{issue.subject}</h2>
		<p class="project">{issue.project.name} <span>({issue.project.identifier})</span></p>
	</header>

	<dl class="facts">
		{#if issue.status}
			<div><dt>Status</dt><dd>{issue.status}</dd></div>
		{/if}
		{#if issue.tracker}
			<div><dt>Tracker</dt><dd>{issue.tracker}</dd></div>
		{/if}
		{#if issue.priority}
			<div><dt>Priority</dt><dd>{issue.priority}</dd></div>
		{/if}
		{#if issue.assignee}
			<div><dt>Assignee</dt><dd>{issue.assignee}</dd></div>
		{/if}
	</dl>

	<section class="block">
		<h3>Description</h3>
		{#if issue.description.trim() !== ''}
			<pre class="description">{issue.description}</pre>
		{:else}
			<p class="muted">No description was provided.</p>
		{/if}
	</section>

	<section class="block">
		<h3>
			Acceptance criteria
			<span class:badge-none={!hasCriteria} class="badge">
				{hasCriteria ? 'from issue heading' : 'not stated'}
			</span>
		</h3>
		{#if hasCriteria}
			<pre class="criteria">{issue.acceptanceCriteria}</pre>
		{:else}
			<p class="muted">
				The issue has no recognisable acceptance-criteria heading. The agent will work from
				the description alone.
			</p>
		{/if}
	</section>

	{#if issue.comments && issue.comments.length > 0}
		<section class="block">
			<h3>Comments</h3>
			<ul class="comments">
				{#each issue.comments as comment (comment.id)}
					<li>
						<p class="comment-meta">
							<strong>{comment.author ?? 'Unknown author'}</strong>
							{#if comment.createdOn}
								<span>{new Date(comment.createdOn).toLocaleString()}</span>
							{/if}
						</p>
						<p class="comment-body">{comment.notes}</p>
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	{#if issue.attachments && issue.attachments.length > 0}
		<section class="block">
			<h3>Attachments</h3>
			<ul class="attachments">
				{#each issue.attachments as attachment (attachment.id)}
					<li class:unavailable={attachment.available === false}>
						<span>{attachment.filename}</span>
						<span class="muted">{attachment.size ?? 0} bytes</span>
						<span class="attachment-state">
							{attachment.available === false ? 'unavailable' : 'available'}
						</span>
					</li>
				{/each}
			</ul>
		</section>
	{/if}
</article>

<style>
	.card {
		background: #fff;
		border: 1px solid #dde2e8;
		border-radius: 0.5rem;
		padding: 1.25rem 1.5rem;
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.card-head h2 {
		margin: 0.15rem 0;
		font-size: 1.25rem;
	}

	.eyebrow {
		margin: 0;
		font-size: 0.8rem;
		letter-spacing: 0.04em;
		text-transform: uppercase;
		color: #5b6b7f;
	}

	.project {
		margin: 0;
		color: #31404f;
	}

	.project span {
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

	h3 {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		margin: 0 0 0.35rem;
		font-size: 0.95rem;
	}

	.badge {
		font-size: 0.7rem;
		font-weight: 500;
		border-radius: 999px;
		padding: 0.1rem 0.5rem;
		background: #e3f2e8;
		color: #1e6b3a;
	}

	.badge-none {
		background: #fdf0d9;
		color: #8a5a07;
	}

	.description,
	.criteria {
		margin: 0;
		padding: 0.75rem;
		background: #f7f8fa;
		border: 1px solid #e4e8ee;
		border-radius: 0.35rem;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
		font-family: inherit;
		font-size: 0.9rem;
	}

	.comments {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.comments li {
		border-left: 3px solid #dde2e8;
		padding-left: 0.75rem;
	}

	.comment-meta {
		margin: 0 0 0.2rem;
		display: flex;
		gap: 0.75rem;
		font-size: 0.8rem;
		color: #6b7a8c;
	}

	.comment-body {
		margin: 0;
		white-space: pre-wrap;
	}

	.attachments {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
	}

	.attachments li {
		display: flex;
		gap: 0.75rem;
		align-items: baseline;
	}

	.attachments li.unavailable {
		color: #8a5a07;
	}

	.attachment-state {
		margin-left: auto;
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}

	.muted {
		color: #6b7a8c;
		margin: 0;
	}

	.attachment-state,
	.muted {
		font-size: 0.85rem;
	}
</style>
