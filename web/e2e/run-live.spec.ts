import { expect, test } from '@playwright/test';

const startedAt = new Date(Date.now() - 5000).toISOString();

const runDetail = {
	runId: 'live-1',
	issueId: 4821,
	issueSubject: 'Invoice totals round incorrectly',
	state: 'implementing',
	stage: 'agent.invoked:1',
	terminal: false,
	repository: { repositoryId: 3, path: 'group/billing-api', baseBranch: 'main', isDefault: true },
	baseBranch: 'main',
	baseRevision: '0123456789abcdef',
	branch: 'agent/issue-4821',
	commitSha: null,
	pushedAt: null,
	mergeRequestUrl: null,
	agent: { name: 'opencode', model: 'fake/model' },
	createdAt: startedAt,
	startedAt,
	endedAt: null,
	elapsedMs: 5000,
	cancelRequestedAt: null,
	workspace: {
		path: '/tmp/workspaces/live-1',
		branch: 'agent/issue-4821',
		baseRevision: '0123456789abcdef',
		createdAt: startedAt,
		hasUnpushedChanges: true,
		changedPaths: ['src/orders/service.go']
	},
	invocations: [
		{
			attemptNo: 1,
			kind: 'implement',
			adapter: 'opencode',
			model: 'fake/model',
			status: 'running',
			reportedStatus: null,
			exitCode: null,
			timedOut: false,
			durationMs: 4000,
			promptSha256: 'abc',
			transcriptAvailable: true
		}
	],
	checks: [],
	report: null,
	timeline: [
		{
			seq: 1,
			at: startedAt,
			type: 'state',
			severity: 'info',
			message: 'the agent is being invoked'
		}
	]
};

const cancelledRun = {
	...runDetail,
	state: 'cancelled',
	stage: 'checks.running',
	terminal: true,
	endedAt: new Date().toISOString(),
	cancelRequestedAt: new Date().toISOString()
};

const streamBody = [
	'id: 2',
	'event: stage',
	'data: {"stage":"checks.running","message":"the worker is inspecting the change"}',
	'',
	'id: 3',
	'event: activity',
	'data: {"message":"Editing src/orders/service.go","severity":"info"}',
	'',
	''
].join('\n');

test('a live run advances, ticks, streams activity, and stops on cancel', async ({ page }) => {
	let cancelPosted = false;

	await page.route('**/api/runs/live-1/cancel', async (route) => {
		cancelPosted = true;
		await route.fulfill({ status: 202, json: cancelledRun });
	});
	await page.route('**/api/runs/live-1/events*', (route) =>
		route.fulfill({
			status: 200,
			headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' },
			body: streamBody
		})
	);
	await page.route('**/api/runs/live-1', (route) => route.fulfill({ status: 200, json: runDetail }));
	await page.route('**/api/health*', (route) =>
		route.fulfill({
			status: 200,
			json: {
				status: 'ok',
				checkedAt: new Date().toISOString(),
				components: [{ name: 'database', status: 'ok', latencyMs: 1 }]
			}
		})
	);

	await page.goto('/runs/live-1');

	await expect(page.getByTestId('run-state')).toHaveText('Implementing');
	await expect(page.getByTestId('stage-stepper')).toContainText('Implementing');

	// The stream advances the stage without a page reload.
	await expect(page.getByTestId('run-stage')).toHaveText('checks.running');
	await expect(page.getByTestId('stage-stepper')).toContainText('Validating');

	await expect(page.getByTestId('activity-feed')).toContainText('Editing src/orders/service.go');

	const timer = page.getByTestId('elapsed-timer');
	const firstReading = await timer.innerText();
	expect(firstReading).toMatch(/\d+s/);
	await page.waitForTimeout(1500);
	await expect(timer).not.toHaveText(firstReading);

	// Cancelling asks first, then reflects the terminal state.
	await page.getByTestId('cancel-button').click();
	await expect(page.getByTestId('cancel-confirm')).toBeVisible();
	expect(cancelPosted).toBe(false);
	await page.getByTestId('cancel-confirm').click();

	await expect(page.getByTestId('run-state')).toHaveText('Cancelled');
	await expect(page.getByTestId('cancel-button')).toHaveCount(0);
	await expect(page.getByTestId('run-summary')).toContainText('cancelled');
	expect(cancelPosted).toBe(true);
});

test('the log viewer loads a tail on demand and reports a pruned file', async ({ page }) => {
	await page.route('**/api/runs/live-1/events*', (route) =>
		route.fulfill({
			status: 200,
			headers: { 'Content-Type': 'text/event-stream' },
			body: ''
		})
	);
	await page.route('**/api/runs/live-1/logs*', (route) => route.fulfill({ status: 410, json: {} }));
	await page.route('**/api/runs/live-1', (route) => route.fulfill({ status: 200, json: runDetail }));
	await page.route('**/api/health*', (route) =>
		route.fulfill({
			status: 200,
			json: { status: 'ok', checkedAt: new Date().toISOString(), components: [] }
		})
	);

	await page.goto('/runs/live-1');
	await page.getByTestId('logs-toggle').click();
	await expect(page.getByTestId('log-viewer')).toContainText('Logs were pruned');
});
