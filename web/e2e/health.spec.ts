import { expect, test } from '@playwright/test';

const healthBody = {
	status: 'degraded',
	checkedAt: '2026-10-26T10:00:00Z',
	components: [
		{
			name: 'database',
			status: 'ok',
			latencyMs: 4,
			detail: 'schema version 1 applied; the database is writable'
		},
		{
			name: 'redmine',
			status: 'failing',
			latencyMs: 21,
			detail: 'the configured credential was rejected',
			remediation: 'Check redmine.api_key and that the credential resolves.'
		},
		{
			name: 'agent',
			status: 'unknown',
			latencyMs: 0,
			detail: 'not probed: repeat the request with deep=true'
		}
	]
};

test.beforeEach(async ({ page }) => {
	await page.route('**/api/health*', (route) => route.fulfill({ status: 200, json: healthBody }));
});

test('lists every component with status and latency', async ({ page }) => {
	await page.goto('/health');

	await expect(page.getByRole('heading', { name: 'Health' })).toBeVisible();
	await expect(page.getByTestId('health-summary')).toContainText('Degraded');

	const components = page.getByTestId('health-component');
	await expect(components).toHaveCount(3);

	const database = components.filter({ hasText: 'database' });
	await expect(database).toContainText('Healthy');
	await expect(database.getByTestId('health-latency')).toHaveText('4 ms');

	const redmine = components.filter({ hasText: 'redmine' });
	await expect(redmine).toContainText('Failing');
	await expect(redmine.getByTestId('health-latency')).toHaveText('21 ms');

	const agent = components.filter({ hasText: 'agent' });
	await expect(agent).toContainText('Unknown');
});

test('shows the remediation for a failing component', async ({ page }) => {
	await page.goto('/health');

	const redmine = page.getByTestId('health-component').filter({ hasText: 'redmine' });
	await expect(redmine.getByTestId('health-remediation')).toContainText('redmine.api_key');
});

test('the deep toggle asks the worker for a deep probe', async ({ page }) => {
	let deepRequests = 0;
	await page.route('**/api/health*', (route) => {
		const url = new URL(route.request().url());
		if (url.searchParams.get('deep') === 'true') {
			deepRequests += 1;
		}
		return route.fulfill({ status: 200, json: healthBody });
	});

	await page.goto('/health');
	await expect(page.getByTestId('health-summary')).toContainText('Degraded');

	await page.getByLabel('Deep probe').check();
	await expect.poll(() => deepRequests).toBeGreaterThan(0);
});

test('refresh reloads the report', async ({ page }) => {
	let requests = 0;
	await page.route('**/api/health*', (route) => {
		requests += 1;
		return route.fulfill({ status: 200, json: healthBody });
	});

	await page.goto('/health');
	await expect(page.getByTestId('health-summary')).toBeVisible();
	const before = requests;

	await page.getByRole('button', { name: 'Refresh' }).click();
	await expect.poll(() => requests).toBeGreaterThan(before);
});
