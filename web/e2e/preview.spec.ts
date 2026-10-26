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

	await expect(page.getByRole('button', { name: 'Confirm and start' })).toBeDisabled();

	await page.getByRole('button', { name: 'Decline' }).click();

	await expect(issueCard).toHaveCount(0);
	await expect(repositoryCard).toHaveCount(0);
	await expect(input).toHaveValue('');
	await expect(input).toBeFocused();
});
