/** SSE event names documented in contracts/openapi.yaml. */
export const RUN_EVENT_TYPES = [
	'state',
	'stage',
	'activity',
	'check',
	'agent',
	'workspace',
	'publish',
	'report',
	'warning',
	'error'
] as const;

export type RunEventType = (typeof RUN_EVENT_TYPES)[number];

export interface SseEventData {
	seq?: number;
	at?: string;
	message?: string;
	severity?: 'info' | 'warn' | 'error';
	stage?: string;
	state?: string;
	[field: string]: unknown;
}

export interface SseEvent {
	id: number;
	type: RunEventType | string;
	data: SseEventData;
}

export type StreamStatus = 'connecting' | 'open' | 'reconnecting' | 'closed';

export interface RunStreamHandlers {
	onEvent?: (event: SseEvent) => void;
	onTerminal?: (state: string) => void;
	onStatus?: (status: StreamStatus) => void;
}

export interface RunStreamOptions {
	/** Reconnect attempts after the first drop. */
	maxAttempts?: number;
	/** Delays between attempts, in milliseconds. */
	backoffMs?: number[];
}

const TERMINAL_STATES = new Set(['completed', 'failed', 'blocked', 'cancelled', 'interrupted']);

const DEFAULT_BACKOFF = [500, 1000, 2000, 4000, 8000];

/**
 * streamRunEvents follows one run over SSE.
 *
 * The browser handles the header on its own automatic reconnect, but a manual
 * reconnect needs the resume point in the query, so this wrapper tracks the
 * last id and rebuilds the URL. Events at or below the last id are ignored,
 * which makes a replay after reconnect idempotent. Returns a teardown function.
 */
export function streamRunEvents(
	runId: string,
	handlers: RunStreamHandlers,
	options: RunStreamOptions = {}
): () => void {
	const maxAttempts = options.maxAttempts ?? 5;
	const backoff = options.backoffMs ?? DEFAULT_BACKOFF;

	let source: EventSource | null = null;
	let timer: ReturnType<typeof setTimeout> | null = null;
	let disposed = false;
	let terminal = false;
	let attempts = 0;
	let lastId = 0;

	const setStatus = (status: StreamStatus) => handlers.onStatus?.(status);

	const teardown = () => {
		if (source) {
			source.close();
			source = null;
		}
		if (timer) {
			clearTimeout(timer);
			timer = null;
		}
	};

	const stop = (status: StreamStatus) => {
		disposed = true;
		teardown();
		setStatus(status);
	};

	const connect = () => {
		if (disposed) {
			return;
		}
		if (typeof EventSource === 'undefined') {
			stop('closed');
			return;
		}

		const query = lastId > 0 ? `?lastEventId=${lastId}` : '';
		source = new EventSource(`/api/runs/${encodeURIComponent(runId)}/events${query}`);
		setStatus(attempts === 0 ? 'connecting' : 'reconnecting');

		source.onopen = () => {
			attempts = 0;
			setStatus('open');
		};

		const dispatch = (message: MessageEvent<string>) => {
			const id = Number.parseInt(message.lastEventId || '', 10);
			if (Number.isFinite(id)) {
				if (id <= lastId) {
					return;
				}
				lastId = id;
			}

			let data: SseEventData = {};
			try {
				data = message.data ? (JSON.parse(message.data) as SseEventData) : {};
			} catch {
				data = { message: message.data };
			}

			const type = (message.type || 'message') as string;
			handlers.onEvent?.({ id: Number.isFinite(id) ? id : lastId, type, data });

			if (type === 'state' && typeof data.state === 'string' && TERMINAL_STATES.has(data.state)) {
				terminal = true;
				stop('closed');
				handlers.onTerminal?.(data.state);
			}
		};

		for (const type of RUN_EVENT_TYPES) {
			source.addEventListener(type, dispatch as EventListener);
		}

		source.onerror = () => {
			if (disposed || terminal) {
				return;
			}
			// Take over from the browser's own retry so the resume point and the
			// attempt budget are ours.
			teardown();
			attempts += 1;
			if (attempts > maxAttempts) {
				stop('closed');
				return;
			}
			setStatus('reconnecting');
			const delay = backoff[Math.min(attempts - 1, backoff.length - 1)];
			timer = setTimeout(connect, delay);
		};
	};

	connect();

	return () => {
		if (disposed) {
			return;
		}
		stop('closed');
	};
}

/** isTerminalState reports whether a run cannot change any more. */
export function isTerminalState(state?: string | null): boolean {
	return typeof state === 'string' && TERMINAL_STATES.has(state);
}
