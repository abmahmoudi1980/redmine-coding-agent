<script lang="ts">
	import type { ValidationResult } from '$lib/api';

	let { checks = [] }: { checks?: ValidationResult[] } = $props();

	function outcomeLabel(outcome: ValidationResult['outcome']): string {
		switch (outcome) {
			case 'passed':
				return 'passed';
			case 'failed':
				return 'failed';
			default:
				return 'not executed';
		}
	}

	function duration(milliseconds?: number): string {
		if (!milliseconds || milliseconds <= 0) {
			return '—';
		}
		if (milliseconds < 1000) {
			return `${milliseconds} ms`;
		}
		return `${(milliseconds / 1000).toFixed(1)} s`;
	}
</script>

{#if checks.length === 0}
	<p class="empty" data-testid="checks-empty">No check has run yet.</p>
{:else}
	<table class="checks" data-testid="check-table">
		<thead>
			<tr>
				<th scope="col">Check</th>
				<th scope="col">Command</th>
				<th scope="col">Exit</th>
				<th scope="col">Duration</th>
				<th scope="col">Result</th>
			</tr>
		</thead>
		<tbody>
			{#each checks as check, index (check.checkName + '-' + index)}
				<tr class={check.outcome}>
					<th scope="row">
						<span class="name">{check.checkName}</span>
						<span class="weight">{check.required ? 'required' : 'optional'}</span>
					</th>
					<td><code>{check.command}</code></td>
					<td>{check.exitCode ?? '—'}</td>
					<td>{duration(check.durationMs)}</td>
					<td>
						<span class="badge {check.outcome}">{outcomeLabel(check.outcome)}</span>
						{#if check.reason}
							<span class="reason">{check.reason}</span>
						{/if}
					</td>
				</tr>
				{#if check.outputExcerpt}
					<tr class="excerpt-row">
						<td colspan="5">
							<details>
								<summary>Output tail</summary>
								<pre>{check.outputExcerpt}</pre>
							</details>
						</td>
					</tr>
				{/if}
			{/each}
		</tbody>
	</table>
{/if}

<style>
	.checks {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.9rem;
	}

	th,
	td {
		text-align: left;
		padding: 0.5rem 0.6rem;
		border-bottom: 1px solid #e6eaf0;
		vertical-align: top;
	}

	thead th {
		font-size: 0.72rem;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: #6b7a8c;
	}

	.name {
		display: block;
		font-weight: 600;
	}

	.weight {
		display: block;
		font-size: 0.72rem;
		font-weight: 500;
		color: #6b7a8c;
	}

	code {
		font-size: 0.82rem;
		word-break: break-word;
	}

	.badge {
		display: inline-block;
		border-radius: 999px;
		padding: 0.1rem 0.55rem;
		font-size: 0.78rem;
		font-weight: 600;
		border: 1px solid transparent;
	}

	.badge.passed {
		background: #edf7ef;
		border-color: #a9d4b4;
		color: #1e6b3a;
	}

	.badge.failed {
		background: #fdf1f0;
		border-color: #e7b7b3;
		color: #7f1d17;
	}

	.badge.not_executed {
		background: #f5f3ee;
		border-color: #d9d2c2;
		color: #6b5a2b;
	}

	.reason {
		display: block;
		margin-top: 0.2rem;
		font-size: 0.78rem;
		color: #6b7a8c;
	}

	.excerpt-row td {
		padding-top: 0;
	}

	pre {
		margin: 0.4rem 0 0;
		max-height: 16rem;
		overflow: auto;
		background: #0f1720;
		color: #dbe4ef;
		border-radius: 0.35rem;
		padding: 0.75rem;
		font-size: 0.78rem;
		white-space: pre-wrap;
		word-break: break-word;
	}

	summary {
		cursor: pointer;
		color: #4a5868;
		font-size: 0.82rem;
	}

	.empty {
		margin: 0;
		color: #6b7a8c;
		font-size: 0.88rem;
	}
</style>
