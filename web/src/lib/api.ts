import type { components } from './api-types';

export type IssuePreview = components['schemas']['IssuePreview'];
export type IssueSnapshot = components['schemas']['IssueSnapshot'];
export type Repository = components['schemas']['Repository'];
export type RepositoryOption = components['schemas']['RepositoryOption'];
export type Problem = components['schemas']['Problem'];
export type HealthReport = components['schemas']['HealthReport'];

/** ApiError carries the RFC 9457 problem document when the worker sent one. */
export class ApiError extends Error {
	readonly status: number;
	readonly problem: Problem;

	constructor(problem: Problem, status: number) {
		super(problem.detail);
		this.name = 'ApiError';
		this.status = status;
		this.problem = problem;
	}
}

function isProblem(value: unknown): value is Problem {
	if (typeof value !== 'object' || value === null) {
		return false;
	}
	const candidate = value as Record<string, unknown>;
	return (
		typeof candidate.type === 'string' &&
		typeof candidate.title === 'string' &&
		typeof candidate.status === 'number' &&
		typeof candidate.code === 'string' &&
		typeof candidate.detail === 'string'
	);
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
	let response: Response;
	try {
		response = await fetch(path, {
			...init,
			headers: { Accept: 'application/json', ...(init?.headers ?? {}) }
		});
	} catch {
		throw new ApiError(
			{
				type: 'about:blank',
				title: 'Worker unreachable',
				status: 0,
				code: 'worker_unreachable',
				detail: 'The worker did not answer. Check that it is running, then try again.'
			},
			0
		);
	}

	const text = await response.text();
	let body: unknown;
	if (text) {
		try {
			body = JSON.parse(text);
		} catch {
			body = text;
		}
	}

	if (!response.ok) {
		if (isProblem(body)) {
			throw new ApiError(body, response.status);
		}
		throw new ApiError(
			{
				type: 'about:blank',
				title: response.statusText || 'Request failed',
				status: response.status,
				code: 'http_error',
				detail:
					typeof body === 'string' && body.trim() !== ''
						? body
						: `The worker returned HTTP ${response.status}.`
			},
			response.status
		);
	}

	return body as T;
}

/** previewIssue retrieves an issue and its destination without side effects. */
export function previewIssue(issueId: number, repositoryId?: number): Promise<IssuePreview> {
	const query = repositoryId ? `?repositoryId=${repositoryId}` : '';
	return request<IssuePreview>(`/api/issues/${issueId}/preview${query}`);
}

/** listRepositories returns the configured mappings, optionally for one project. */
export async function listRepositories(project?: string): Promise<Repository[]> {
	const query = project ? `?project=${encodeURIComponent(project)}` : '';
	const payload = await request<{ items: Repository[] }>(`/api/repositories${query}`);
	return payload.items;
}

/** getHealth reports worker and dependency liveness. */
export function getHealth(): Promise<HealthReport> {
	return request<HealthReport>('/api/health');
}
