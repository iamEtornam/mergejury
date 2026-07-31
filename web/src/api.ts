// Typed client for the revu HTTP API.

export interface Run {
  id: number
  repo: string
  pr_number: number
  head_sha: string
  base_sha: string
  trigger: string
  status: string
  review_event: string
  review_event_reason: string
  started_at: string
  finished_at: string | null
  total_cost_usd: number
  findings_produced: number
  comments_posted: number
}

export interface Finding {
  reviewer_id: string
  lens: string
  path: string
  line: number
  start_line: number | null
  category: string
  severity: string
  title: string
  body: string
  suggested_patch: string | null
  evidence: string[]
  confidence: string
}

export interface StoredFinding {
  id: number
  adapter_run_id: number
  adapter_id: string
  lens: string
  finding: Finding
  kept: boolean
  drop_reason: string
}

export interface AdapterRun {
  id: number
  run_id: number
  adapter_id: string
  lens: string
  model: string
  status: string
  duration_ms: number
  cost_usd: number
  input_tokens: number
  output_tokens: number
  raw_output: string
  error: string
}

export interface Cluster {
  id: number
  run_id: number
  path: string
  line: number
  category: string
  supporter_count: number
  finding_ids: number[]
}

export interface Verification {
  id: number
  cluster_id: number
  kind: string
  command: string
  exit_code: number
  output: string
  conclusion: string
}

export interface Challenge {
  id: number
  cluster_id: number
  model: string
  argument: string
  could_argue: boolean
}

export interface Verdict {
  id: number
  cluster_id: number
  verdict: string
  reason: string
  final_severity: string
  final_body: string
  final_patch: string | null
  posted_comment_id: number | null
  posted_at: string | null
}

export interface RunDetail {
  run: Run
  adapter_runs: AdapterRun[]
  findings: StoredFinding[]
  clusters: Cluster[]
  verifications: Verification[]
  challenges: Challenge[]
  verdicts: Verdict[]
}

export interface ProbeResult {
  adapter_id: string
  ok: boolean
  detail: string
  remediation: string
}

export interface AdapterStats {
  adapter_id: string
  lens: string
  runs: number
  findings_produced: number
  findings_kept: number
  published: number
  resolved: number
  dismissed: number
  median_latency_ms: number
  total_cost_usd: number
  cost_per_published: number
}

export interface RunEvent {
  run_id: number
  type: string
  payload: unknown
  at: string
}

async function get<T>(url: string): Promise<T> {
  const res = await fetch(url)
  if (!res.ok) throw new Error(`${url}: ${res.status} ${await res.text()}`)
  return res.json()
}

export const api = {
  runs: (): Promise<Run[]> => get('/api/runs'),
  run: (id: number | string): Promise<RunDetail> => get(`/api/runs/${id}`),
  replay: (id: number) => fetch(`/api/runs/${id}/replay`, { method: 'POST' }),
  probe: (): Promise<ProbeResult[]> => get('/api/adapters/probe'),
  stats: (): Promise<AdapterStats[]> => get('/api/stats'),
  prompts: (): Promise<string[]> => get('/api/prompts'),
  prompt: (name: string): Promise<{ name: string; content: string; committed: string }> =>
    get(`/api/prompts/${name}`),
  savePrompt: (name: string, content: string) =>
    fetch(`/api/prompts/${name}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content }),
    }),
  startRun: (repo: string, pr: number, dryRun: boolean) =>
    fetch('/api/runs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ repo, pr_number: pr, dry_run: dryRun }),
    }),
}

// Subscribe to run events over SSE. Returns a cleanup function.
export function subscribe(runId: number | null, onEvent: (e: RunEvent) => void): () => void {
  const url = runId ? `/api/runs/${runId}/events` : '/api/events'
  const es = new EventSource(url)
  const types = [
    'run_started', 'adapter_started', 'adapter_finished', 'adapter_retry',
    'validated', 'clustered', 'cluster', 'challenge', 'verdict', 'posted',
    'run_finished', 'run_error', 'replay_started', 'replay_finished',
  ]
  for (const t of types) {
    es.addEventListener(t, (ev) => onEvent(JSON.parse((ev as MessageEvent).data)))
  }
  return () => es.close()
}

export const sevGlyph: Record<string, string> = {
  blocker: '◆', major: '▲', minor: '●', nit: '·',
}

export function fmtCost(v: number): string {
  return v > 0 ? `$${v.toFixed(4)}` : '—'
}

export function fmtDuration(ms: number): string {
  if (ms <= 0) return '—'
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(1)}s`
}

export function shortSha(sha: string): string {
  return sha ? sha.slice(0, 10) : '—'
}
