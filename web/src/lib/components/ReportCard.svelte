<script lang="ts">
	import type { Report } from '$lib/api';

	let {
		report = null,
		commitSha = '',
		mergeRequestUrl = ''
	}: {
		report?: Report | null;
		commitSha?: string;
		mergeRequestUrl?: string;
	} = $props();

	type Block =
		| { kind: 'heading'; level: 3 | 4; text: string }
		| { kind: 'paragraph'; text: string }
		| { kind: 'list'; items: string[] }
		| { kind: 'table'; headers: string[]; rows: string[][] }
		| { kind: 'rule' };

	const blocks = $derived(report ? parseMarkdown(report.bodyMarkdown) : []);

	function deliveryLabel(status: Report['deliveryStatus']): string {
		switch (status) {
			case 'delivered':
				return 'comment posted to Redmine';
			case 'failed':
				return 'Redmine comment failed';
			case 'pending':
				return 'not sent yet';
			default:
				return 'comment skipped';
		}
	}

	function shortRevision(value?: string | null): string {
		return value ? value.slice(0, 12) : '—';
	}

	function cleanInline(text: string): string {
		return text.replaceAll('**', '').replace(/`([^`]+)`/g, '$1');
	}

	function splitRow(line: string): string[] {
		return line
			.replace(/^\|/, '')
			.replace(/\|$/, '')
			.split('|')
			.map((cell) => cleanInline(cell.trim()));
	}

	function isSeparatorRow(cells: string[]): boolean {
		return cells.length > 0 && cells.every((cell) => /^-+$/.test(cell.replaceAll(':', '')));
	}

	function parseMarkdown(markdown: string): Block[] {
		const lines = markdown.replaceAll('\r\n', '\n').split('\n');
		const parsed: Block[] = [];
		let index = 0;

		while (index < lines.length) {
			const trimmed = lines[index].trim();
			if (trimmed === '') {
				index++;
				continue;
			}
			if (trimmed === '---') {
				parsed.push({ kind: 'rule' });
				index++;
				continue;
			}

			const heading = /^(#{2,4})\s+(.*)$/.exec(trimmed);
			if (heading) {
				parsed.push({
					kind: 'heading',
					level: heading[1].length === 2 ? 3 : 4,
					text: cleanInline(heading[2])
				});
				index++;
				continue;
			}

			if (trimmed.startsWith('|')) {
				const rows: string[][] = [];
				while (index < lines.length && lines[index].trim().startsWith('|')) {
					rows.push(splitRow(lines[index].trim()));
					index++;
				}
				const headers = rows.shift() ?? [];
				parsed.push({ kind: 'table', headers, rows: rows.filter((row) => !isSeparatorRow(row)) });
				continue;
			}

			if (/^[-*]\s+/.test(trimmed)) {
				const items: string[] = [];
				while (index < lines.length && /^[-*]\s+/.test(lines[index].trim())) {
					items.push(cleanInline(lines[index].trim().replace(/^[-*]\s+/, '')));
					index++;
				}
				parsed.push({ kind: 'list', items });
				continue;
			}

			const paragraph: string[] = [];
			while (index < lines.length) {
				const next = lines[index].trim();
				if (
					next === '' ||
					next === '---' ||
					next.startsWith('#') ||
					next.startsWith('|') ||
					/^[-*]\s+/.test(next)
				) {
					break;
				}
				paragraph.push(next);
				index++;
			}
			if (paragraph.length > 0) {
				parsed.push({ kind: 'paragraph', text: cleanInline(paragraph.join(' ')) });
			}
		}
		return parsed;
	}
</script>

{#if report}
	<section class="report-card" data-testid="report-card">
		<header class="head">
			<h2>Completion report</h2>
			<span class="delivery {report.deliveryStatus}" data-testid="delivery-status">
				{deliveryLabel(report.deliveryStatus)}
			</span>
		</header>

		{#if report.deliveryStatus === 'failed'}
			<p class="warning" role="alert" data-testid="delivery-warning">
				The report could not be posted to Redmine, so a human may not have seen it there yet:
				<span class="detail">{report.deliveryError || 'the comment was rejected.'}</span>
				The text below is preserved in the worker — the implementation, push, and merge request are unaffected.
			</p>
		{/if}

		<dl class="links">
			<div>
				<dt>Commit</dt>
				<dd><code>{shortRevision(report.commitSha || commitSha)}</code></dd>
			</div>
			<div>
				<dt>Merge request</dt>
				<dd>
					{#if report.mergeRequestUrl || mergeRequestUrl}
						<a
							href={report.mergeRequestUrl || mergeRequestUrl}
							target="_blank"
							rel="noreferrer"
							data-testid="report-merge-request"
						>
							{report.mergeRequestUrl || mergeRequestUrl}
						</a>
					{:else}
						<span class="muted">not created</span>
					{/if}
				</dd>
			</div>
		</dl>

		<div class="body" data-testid="report-body">
			{#each blocks as block, index (index)}
				{#if block.kind === 'heading'}
					{#if block.level === 3}
						<h3>{block.text}</h3>
					{:else}
						<h4>{block.text}</h4>
					{/if}
				{:else if block.kind === 'paragraph'}
					<p>{block.text}</p>
				{:else if block.kind === 'list'}
					<ul>
						{#each block.items as item, itemIndex (itemIndex)}
							<li>{item}</li>
						{/each}
					</ul>
				{:else if block.kind === 'table'}
					<div class="table-wrap">
						<table>
							<thead>
								<tr>
									{#each block.headers as header, headerIndex (headerIndex)}
										<th scope="col">{header}</th>
									{/each}
								</tr>
							</thead>
							<tbody>
								{#each block.rows as row, rowIndex (rowIndex)}
									<tr>
										{#each row as cell, cellIndex (cellIndex)}
											<td>{cell}</td>
										{/each}
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{:else}
					<hr />
				{/if}
			{/each}
		</div>
	</section>
{:else}
	<p class="empty" data-testid="report-empty">No report was written for this run yet.</p>
{/if}

<style>
	.report-card {
		display: flex;
		flex-direction: column;
		gap: 0.85rem;
	}

	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
		flex-wrap: wrap;
	}

	.head h2 {
		margin: 0;
		font-size: 1rem;
	}

	.delivery {
		border-radius: 999px;
		padding: 0.15rem 0.7rem;
		font-size: 0.78rem;
		font-weight: 600;
		border: 1px solid #c3ccd7;
		background: #f5f7fa;
		color: #35455a;
	}

	.delivery.delivered {
		background: #edf7ef;
		border-color: #a9d4b4;
		color: #1e6b3a;
	}

	.delivery.failed {
		background: #fdf1f0;
		border-color: #e7b7b3;
		color: #7f1d17;
	}

	.delivery.skipped,
	.delivery.pending {
		background: #f5f3ee;
		border-color: #d9d2c2;
		color: #6b5a2b;
	}

	.warning {
		margin: 0;
		border: 1px solid #e7b7b3;
		background: #fdf1f0;
		border-radius: 0.45rem;
		padding: 0.65rem 0.85rem;
		color: #5d1a15;
		font-size: 0.85rem;
	}

	.warning .detail {
		display: block;
		margin: 0.25rem 0;
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.78rem;
	}

	.links {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem 1.75rem;
		margin: 0;
	}

	.links div {
		display: flex;
		flex-direction: column;
		gap: 0.15rem;
		min-width: 0;
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
		overflow-wrap: anywhere;
	}

	.body {
		border: 1px solid #e6eaf0;
		border-radius: 0.45rem;
		padding: 0.75rem 1rem;
		background: #fbfcfd;
		font-size: 0.88rem;
	}

	.body :global(h3) {
		margin: 0.9rem 0 0.4rem;
		font-size: 0.92rem;
	}

	.body :global(h3:first-child) {
		margin-top: 0;
	}

	.body :global(h4) {
		margin: 0.7rem 0 0.3rem;
		font-size: 0.86rem;
	}

	.body :global(p) {
		margin: 0.35rem 0;
		color: #35455a;
	}

	.body :global(ul) {
		margin: 0.35rem 0;
		padding-left: 1.15rem;
	}

	.body :global(li) {
		margin: 0.15rem 0;
	}

	.body :global(hr) {
		border: 0;
		border-top: 1px solid #e6eaf0;
		margin: 0.8rem 0;
	}

	.table-wrap {
		overflow-x: auto;
	}

	.body :global(table) {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.82rem;
	}

	.body :global(th),
	.body :global(td) {
		text-align: left;
		padding: 0.35rem 0.5rem;
		border-bottom: 1px solid #e6eaf0;
	}

	.body :global(thead th) {
		font-size: 0.72rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #6b7a8c;
	}

	code {
		font-size: 0.82rem;
		word-break: break-word;
	}

	.muted {
		color: #6b7a8c;
		font-weight: 500;
	}

	.empty {
		margin: 0;
		color: #6b7a8c;
		font-size: 0.88rem;
	}
</style>
