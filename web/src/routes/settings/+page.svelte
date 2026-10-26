<script lang="ts">
	import { onMount } from 'svelte';
	import {
		ApiError,
		getEffectiveConfig,
		getMeta,
		type EffectiveConfig,
		type Meta,
		type Problem
	} from '$lib/api';

	let config = $state<EffectiveConfig | null>(null);
	let meta = $state<Meta | null>(null);
	let problem = $state<Problem | null>(null);
	let loading = $state(false);

	onMount(() => {
		void load();
	});

	async function load() {
		loading = true;
		problem = null;
		try {
			const [loaded, identity] = await Promise.all([getEffectiveConfig(), getMeta()]);
			config = loaded;
			meta = identity;
		} catch (error) {
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
			detail: 'The configuration could not be loaded.'
		};
	}

	function loadedAt(value?: string): string {
		if (!value) {
			return '—';
		}
		const parsed = new Date(value);
		return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
	}
</script>

<svelte:head>
	<title>Settings — RedmineCodingAgent</title>
</svelte:head>

<section class="panel">
	<h1>Settings</h1>
	<p class="restart-note" data-testid="config-restart-note">
		This view is read-only. Edit <code>config.yaml</code> and restart the worker; every change is applied
		on startup and a new configuration profile is recorded.
	</p>

	{#if problem}
		<div class="problem" role="alert" data-testid="problem">{problem.detail}</div>
	{/if}

	{#if loading && !config}
		<p>Loading…</p>
	{/if}

	{#if config}
		<dl class="facts">
			<div>
				<dt>Schema version</dt>
				<dd>{config.schemaVersion}</dd>
			</div>
			<div>
				<dt>Content hash</dt>
				<dd><code data-testid="config-hash">{config.contentHash}</code></dd>
			</div>
			<div>
				<dt>Loaded at</dt>
				<dd data-testid="config-loaded-at">{loadedAt(config.loadedAt)}</dd>
			</div>
			{#if meta}
				<div>
					<dt>Worker</dt>
					<dd>{meta.version} · {meta.os}/{meta.arch}</dd>
				</div>
			{/if}
		</dl>

		<h2>Effective configuration</h2>
		<p class="hint">
			Every credential is shown as its reference (<code>env:NAME</code> or <code>file:/path</code>);
			the resolved value never leaves the worker.
		</p>
		<pre data-testid="config-document">{JSON.stringify(config.redacted, null, 2)}</pre>
	{/if}
</section>

<style>
	.panel {
		background: #fff;
		border: 1px solid #dde3ea;
		border-radius: 0.75rem;
		padding: 1.5rem;
	}

	h1 {
		margin: 0 0 0.35rem;
		font-size: 1.35rem;
	}

	h2 {
		margin: 1.5rem 0 0.35rem;
		font-size: 1.05rem;
	}

	.restart-note,
	.hint {
		margin: 0 0 1rem;
		color: #55606d;
		font-size: 0.9rem;
		max-width: 52rem;
	}

	.facts {
		display: flex;
		flex-wrap: wrap;
		gap: 1.5rem;
		margin: 0;
		padding: 0.9rem 1rem;
		border: 1px solid #e3e8ee;
		border-radius: 0.6rem;
		background: #fbfcfd;
	}

	.facts div {
		display: flex;
		flex-direction: column;
		gap: 0.15rem;
	}

	.facts dt {
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: #6b7684;
	}

	.facts dd {
		margin: 0;
		font-size: 0.9rem;
		color: #1b1f24;
	}

	.problem {
		margin: 1rem 0;
		padding: 0.75rem 1rem;
		border-radius: 0.5rem;
		background: #fdecea;
		border: 1px solid #f3b4ab;
		color: #8b1e12;
	}

	pre {
		margin: 0;
		padding: 1rem;
		background: #0f172a;
		color: #e2e8f0;
		border-radius: 0.6rem;
		overflow: auto;
		font-size: 0.82rem;
		line-height: 1.5;
		max-height: 34rem;
	}
</style>
