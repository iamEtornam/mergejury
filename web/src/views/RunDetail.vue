<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api, fmtCost, fmtDuration, sevGlyph, shortSha, type RunDetail } from '../api'

const route = useRoute()
const detail = ref<RunDetail | null>(null)
const error = ref('')
const replaying = ref(false)

// Selection: clicking a finding, cluster, or verdict highlights the whole
// path it took through the funnel.
const selCluster = ref<number | null>(null)

async function load() {
  try {
    detail.value = await api.run(route.params.id as string)
  } catch (e) {
    error.value = String(e)
  }
}
onMounted(load)

const findingsByAdapterRun = computed(() => {
  const m = new Map<number, typeof d.findings>()
  const d = detail.value!
  for (const f of d.findings) {
    if (!m.has(f.adapter_run_id)) m.set(f.adapter_run_id, [])
    m.get(f.adapter_run_id)!.push(f)
  }
  return m
})

function clusterOfFinding(findingId: number): number | null {
  for (const c of detail.value?.clusters ?? []) {
    if (c.finding_ids.includes(findingId)) return c.id
  }
  return null
}

function verdictOfCluster(clusterId: number) {
  return detail.value?.verdicts.find((v) => v.cluster_id === clusterId) ?? null
}
function challengeOfCluster(clusterId: number) {
  return detail.value?.challenges.find((c) => c.cluster_id === clusterId) ?? null
}
function verificationsOfCluster(clusterId: number) {
  return detail.value?.verifications.filter((v) => v.cluster_id === clusterId) ?? []
}

function select(clusterId: number | null) {
  selCluster.value = selCluster.value === clusterId ? null : clusterId
}

const linkedFindings = computed<Set<number>>(() => {
  const s = new Set<number>()
  if (selCluster.value == null) return s
  const c = detail.value?.clusters.find((c) => c.id === selCluster.value)
  for (const id of c?.finding_ids ?? []) s.add(id)
  return s
})

const posted = computed(() =>
  (detail.value?.verdicts ?? []).filter((v) => v.posted_comment_id != null),
)

async function replay() {
  if (!detail.value) return
  replaying.value = true
  await api.replay(detail.value.run.id)
  // Adjudication rebuilds server-side; poll once after a beat.
  setTimeout(async () => {
    await load()
    replaying.value = false
  }, 3000)
}

function dropLabel(f: { kept: boolean; drop_reason: string }): string {
  if (!f.kept) return `dropped: ${f.drop_reason}`
  if (f.drop_reason) return `demoted: ${f.drop_reason}`
  return ''
}

function clusterAnchor(c: { path: string; line: number }): string {
  return `${c.path}:${c.line}`
}
</script>

<template>
  <p v-if="error" class="err">{{ error }}</p>
  <template v-else-if="detail">
    <h1>
      Run {{ detail.run.id }}
      <span class="dim" v-if="detail.run.repo"> · {{ detail.run.repo }}#{{ detail.run.pr_number }}</span>
      <span class="dim" v-else> · local</span>
    </h1>
    <div class="kv">
      <span><span class="k">head</span><span class="v">{{ shortSha(detail.run.head_sha) }}</span></span>
      <span><span class="k">status</span><span class="v" :class="detail.run.status === 'completed' ? 'status-ok' : 'status-bad'">{{ detail.run.status }}</span></span>
      <span v-if="detail.run.review_event"><span class="k">event</span><span class="v">{{ detail.run.review_event }}</span></span>
      <span><span class="k">cost</span><span class="v">{{ fmtCost(detail.run.total_cost_usd) }}</span></span>
      <span><button @click="replay" :disabled="replaying">{{ replaying ? 'replaying…' : 'replay adjudication' }}</button></span>
    </div>
    <p v-if="detail.run.review_event_reason" class="dim" style="margin-top:-8px">{{ detail.run.review_event_reason }}</p>

    <div class="funnel">
      <!-- stage 1: adapters -->
      <div class="stage">
        <div class="stage-title">ADAPTERS <span class="count">{{ detail.adapter_runs.length }}</span></div>
        <div v-for="ar in detail.adapter_runs" :key="ar.id" class="panel" style="margin-bottom: 10px">
          <div class="head" style="display:flex; justify-content:space-between; align-items:baseline">
            <strong>{{ ar.adapter_id }}</strong>
            <span class="mono" :class="ar.status === 'ok' ? 'status-ok' : 'status-bad'">{{ ar.status }}</span>
          </div>
          <div class="meta faint mono" style="margin: 2px 0 8px">
            {{ ar.lens }} · {{ fmtDuration(ar.duration_ms) }} · {{ fmtCost(ar.cost_usd) }}
          </div>
          <div v-if="ar.error" class="err">{{ ar.error }}</div>
          <div
            v-for="f in findingsByAdapterRun.get(ar.id) ?? []"
            :key="f.id"
            class="card selectable"
            :class="{ dropped: !f.kept, linked: linkedFindings.has(f.id) }"
            :title="dropLabel(f)"
            @click="select(clusterOfFinding(f.id))"
          >
            <div class="head">
              <span class="sev" :class="'sev-' + f.finding.severity">{{ sevGlyph[f.finding.severity] ?? '?' }} {{ f.finding.severity }}</span>
              <span class="anchor">{{ f.finding.path }}:{{ f.finding.line }}</span>
            </div>
            <div class="title">{{ f.finding.title }}</div>
            <div v-if="dropLabel(f)" class="meta">{{ dropLabel(f) }}</div>
          </div>
          <details class="raw">
            <summary>raw output</summary>
            <pre>{{ ar.raw_output || '(empty)' }}</pre>
          </details>
        </div>
      </div>

      <!-- stage 2: clusters -->
      <div class="stage">
        <div class="stage-title">CLUSTERS <span class="count">{{ detail.clusters.length }}</span></div>
        <div
          v-for="c in detail.clusters"
          :key="c.id"
          class="card selectable"
          :class="{ linked: selCluster === c.id }"
          @click="select(c.id)"
        >
          <div class="head">
            <span class="mono">{{ c.category }}</span>
            <span class="anchor">{{ clusterAnchor(c) }}</span>
          </div>
          <div class="meta">{{ c.supporter_count }} supporter(s) · {{ c.finding_ids.length }} finding(s)</div>
          <div v-if="challengeOfCluster(c.id)" class="meta">
            challenge: {{ challengeOfCluster(c.id)!.could_argue ? 'argued false positive' : 'could not argue' }}
          </div>
          <div v-for="v in verificationsOfCluster(c.id)" :key="v.id" class="meta">
            {{ v.kind }}: {{ v.conclusion }}
          </div>
        </div>
        <p v-if="!detail.clusters.length" class="empty">No clusters.</p>
      </div>

      <!-- stage 3: verdicts -->
      <div class="stage">
        <div class="stage-title">VERDICTS <span class="count">{{ detail.verdicts.length }}</span></div>
        <div
          v-for="v in detail.verdicts"
          :key="v.id"
          class="card selectable"
          :class="{ linked: selCluster === v.cluster_id }"
          @click="select(v.cluster_id)"
        >
          <div class="head">
            <strong :class="v.verdict === 'publish' ? 'status-ok' : v.verdict === 'drop' ? 'faint' : ''">{{ v.verdict }}</strong>
            <span v-if="v.final_severity" class="sev" :class="'sev-' + v.final_severity">{{ sevGlyph[v.final_severity] }} {{ v.final_severity }}</span>
          </div>
          <div class="meta">{{ v.reason }}</div>
          <div v-if="v.final_body" class="title dim" style="margin-top:4px">{{ v.final_body }}</div>
        </div>
        <p v-if="!detail.verdicts.length" class="empty">No verdicts.</p>
      </div>

      <!-- stage 4: posted -->
      <div class="stage">
        <div class="stage-title">POSTED <span class="count">{{ posted.length }}</span></div>
        <div
          v-for="v in posted"
          :key="v.id"
          class="card selectable"
          :class="{ linked: selCluster === v.cluster_id }"
          @click="select(v.cluster_id)"
        >
          <div class="head">
            <span class="sev" :class="'sev-' + v.final_severity">{{ sevGlyph[v.final_severity] }} {{ v.final_severity }}</span>
            <span class="anchor mono">review {{ v.posted_comment_id }}</span>
          </div>
          <div class="title">{{ v.final_body }}</div>
          <div class="meta">{{ v.posted_at }}</div>
        </div>
        <p v-if="!posted.length" class="empty">Nothing posted (dry run, local run, or all dropped).</p>
      </div>
    </div>
  </template>
</template>
