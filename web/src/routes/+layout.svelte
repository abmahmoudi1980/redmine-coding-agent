<script lang="ts">
	import { onMount } from 'svelte';
	import { resolve } from '$app/paths';
	import { getHealth } from '$lib/api';

	let { children } = $props();

	let workerStatus = $state<'checking' | 'ok' | 'degraded' | 'unknown'>('checking');

	onMount(async () => {
		try {
			const report = await getHealth();
			workerStatus = report.status;
		} catch {
			workerStatus = 'unknown';
		}
	});
</script>

<header class="app-header">
	<a class="brand" href={resolve('/')}>Redmine Coding Agent</a>
	<nav>
		<a href={resolve('/')}>Issue preview</a>
	</nav>
	<p class="worker" data-testid="worker-status" aria-live="polite">
		<span class:ok={workerStatus === 'ok'} class:degraded={workerStatus === 'degraded'}></span>
		Worker: {workerStatus}
	</p>
</header>

<main>
	{@render children()}
</main>

<style>
	:global(body) {
		margin: 0;
		font-family:
			system-ui,
			-apple-system,
			'Segoe UI',
			sans-serif;
		color: #1b1f24;
		background: #f6f7f9;
	}

	.app-header {
		display: flex;
		gap: 1.5rem;
		align-items: center;
		padding: 0.75rem 1.5rem;
		background: #10233f;
		color: #fff;
	}

	.brand {
		font-weight: 600;
		color: #fff;
		text-decoration: none;
	}

	nav {
		display: flex;
		gap: 1rem;
	}

	nav a {
		color: #c9d7ea;
		text-decoration: none;
		font-size: 0.9rem;
	}

	nav a:hover {
		color: #fff;
	}

	.worker {
		margin-left: auto;
		display: flex;
		align-items: center;
		gap: 0.4rem;
		font-size: 0.8rem;
		color: #c9d7ea;
	}

	.worker span {
		display: inline-block;
		width: 0.5rem;
		height: 0.5rem;
		border-radius: 50%;
		background: #8b98a8;
	}

	.worker span.ok {
		background: #3ddc84;
	}

	.worker span.degraded {
		background: #f0b429;
	}

	main {
		max-width: 72rem;
		margin: 0 auto;
		padding: 1.5rem;
	}
</style>
