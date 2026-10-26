import { expect, test } from '@playwright/test';

const previewBody = {
	issue: {
		issueId: 4821,
		subject: 'Invoice totals round incorrectly',
		description:
			'Totals are rounded down.\n\n## Acceptance Criteria\n- totals round half up\n- existing invoices still load\n',
		acceptanceCriteria: '- totals round half up\n- existing invoices still load',
		criteriaSource: 'heading',
		project: { identifier: 'billing', name: 'Billing' },
		status: 'New',
		tracker: 'Feature',
		priority: 'Normal',
		assignee: 'Operator',
		comments: [
			{
				id: 1,
				author: 'Reporter',
				createdOn: '2026-10-01T09:00:00Z',
				notes: 'Seen on invoice 1001.'
			}
		],
		attachments: [{ id: 55, filename: 'screen.txt', size: 12, available: true }],
		contentHash: 'sha256-fixture',
		fetchedAt: '2026-10-26T10:00:00Z'
	},
	repository: {
		repositoryId: 3,
		path: 'group/billing-api',
		displayName: 'Billing API',
		baseBranch: 'main',
		isDefault: true
	},
	baseBranch: 'main',
	warnings: [],
	startedAt: '2026-10-26T10:00:00Z'
};

const runBody = {
	runId: 'run-1',
	issueId: 4821,
	issueSubject: 'Invoice totals round incorrectly',
	state: 'completed',
	stage: 'commit.created',
	terminal: true,
	repository: { repositoryId: 3, path: 'group/billing-api', baseBranch: 'main', isDefault: true },
	baseBranch: 'main',
	baseRevision: '0123456789abcdef',
	branch: 'agent/issue-4821',
	commitSha: 'fedcba9876543210',
	agent: { name: 'opencode', model: 'fake/model' },
	createdAt: '2026-10-26T10:00:00Z',
	startedAt: '2026-10-26T10:00:01Z',
	endedAt: '2026-10-26T10:01:00Z',
	elapsedMs: 59000
};

const runDetailBody = {
	...runBody,
	workspace: {
		path: '/tmp/workspaces/run-1',
		branch: 'agent/issue-4821',
		baseRevision: '0123456789abcdef',
		createdAt: '2026-10-26T10:00:01Z',
		hasUnpushedChanges: true,
		changedPaths: ['internal/invoice/total.go', 'internal/invoice/total_test.go']
	},
	invocations: [
		{
			attemptNo: 1,
			kind: 'implement',
			adapter: 'opencode',
			model: 'fake/model',
			status: 'succeeded',
			reportedStatus: 'done',
			exitCode: 0,
			timedOut: false,
			durationMs: 12000,
			promptSha256: 'abc',
			transcriptAvailable: true
		}
	],
	checks: [
		{
			checkName: 'unit-tests',
			command: 'go test ./...',
			required: true,
			outcome: 'passed',
			exitCode: 0,
			durationMs: 900,
			outputExcerpt: 'ok  billing/internal/invoice'
		}
	],
	timeline: [
		{
			seq: 1,
			at: '2026-10-26T10:00:01Z',
			type: 'state',
			severity: 'info',
			message: 'the agent is being invoked'
		}
	]
};

test.beforeEach(async ({ page }) => {
	await page.route('**/api/issues/4821/preview*', (route) =>
		route.fulfill({ status: 200, json: previewBody })
	);
	await page.route('**/api/health*', (route) =>
		route.fulfill({
			status: 200,
			json: {
				status: 'ok',
				checkedAt: '2026-10-26T10:00:00Z',
				components: [{ name: 'database', status: 'ok', latencyMs: 1 }]
			}
		})
	);
});

test('previews an issue and declines back to an empty input', async ({ page }) => {
	await page.goto('/');

	const input = page.getByLabel('Issue number');
	await expect(input).toBeVisible();
	await input.fill('4821');
	await page.getByRole('button', { name: 'Preview issue' }).click();

	const issueCard = page.getByTestId('issue-card');
	await expect(issueCard).toBeVisible();
	await expect(issueCard).toContainText('Invoice totals round incorrectly');
	await expect(issueCard).toContainText('Billing');
	await expect(issueCard).toContainText('New');
	await expect(issueCard).toContainText('totals round half up');
	await expect(issueCard).toContainText('Seen on invoice 1001.');

	const repositoryCard = page.getByTestId('repository-card');
	await expect(repositoryCard).toBeVisible();
	await expect(repositoryCard).toContainText('group/billing-api');
	await expect(repositoryCard).toContainText('main');

	await expect(page.getByRole('button', { name: 'Confirm and start' })).toBeEnabled();

	await page.getByRole('button', { name: 'Decline' }).click();

	await expect(issueCard).toHaveCount(0);
	await expect(repositoryCard).toHaveCount(0);
	await expect(input).toHaveValue('');
	await expect(input).toBeFocused();
});

test('confirms a run and shows the verified result', async ({ page }) => {
	await page.route('**/api/runs', (route) =>
		route.fulfill({ status: 201, headers: { Location: '/api/runs/run-1' }, json: runBody })
	);
	await page.route('**/api/runs/run-1', (route) =>
		route.fulfill({ status: 200, json: runDetailBody })
	);

	await page.goto('/');
	await page.getByLabel('Issue number').fill('4821');
	await page.getByRole('button', { name: 'Preview issue' }).click();
	await page.getByRole('button', { name: 'Confirm and start' }).click();

	await expect(page).toHaveURL(/\/runs\/run-1$/);
	await expect(page.getByTestId('run-state')).toHaveText('Completed');
	await expect(page.getByTestId('run-stage')).toHaveText('commit.created');
	await expect(page.getByTestId('changed-files')).toContainText('internal/invoice/total.go');

	const table = page.getByTestId('check-table');
	await expect(table).toBeVisible();
	await expect(table).toContainText('unit-tests');
	await expect(table).toContainText('go test ./...');
	await expect(table).toContainText('required');
	await expect(table).toContainText('passed');

	await expect(page.getByTestId('invocations')).toContainText('implement');
	await expect(page.getByTestId('timeline')).toContainText('the agent is being invoked');
	await expect(page.getByText('Nothing is merged')).toBeVisible();
});

test('a blocked start links to the run that already owns the issue', async ({ page }) => {
	await page.route('**/api/runs', (route) =>
		route.fulfill({
			status: 409,
			contentType: 'application/problem+json',
			json: {
				type: 'https://redmine-agent.local/problems/active_run_exists',
				title: 'An active run already exists',
				status: 409,
				code: 'active_run_exists',
				detail: 'Issue 4821 already has run run-9 in stage implementing.',
				activeRun: { runId: 'run-9', stage: 'implementing', state: 'implementing' }
			}
		})
	);

	await page.goto('/');
	await page.getByLabel('Issue number').fill('4821');
	await page.getByRole('button', { name: 'Preview issue' }).click();
	await page.getByRole('button', { name: 'Confirm and start' }).click();

	const problem = page.getByTestId('problem');
	await expect(problem).toContainText('active run');
	await expect(page.getByTestId('blocking-run-link')).toHaveAttribute('href', '/runs/run-9');
});
