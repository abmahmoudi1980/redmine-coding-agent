<script lang="ts">
	import { resolve } from '$app/paths';

	let { children } = $props();
</script>

<header class="app-header">
	<a class="brand" href={resolve('/')}>Redmine Coding Agent</a>
	<nav>
		<a href={resolve('/')}>Issue preview</a>
	</nav>
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

	main {
		max-width: 72rem;
		margin: 0 auto;
		padding: 1.5rem;
	}
</style>
