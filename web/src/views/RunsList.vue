<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, fmtCost, shortSha, type Run } from '../api'

const router = useRouter()
const runs = ref<Run[]>([])
const error = ref('')

onMounted(async () => {
  try {
    runs.value = await api.runs()
  } catch (e) {
    error.value = String(e)
  }
})

function duration(r: Run): string {
  if (!r.finished_at) return 'running'
  const ms = new Date(r.finished_at).getTime() - new Date(r.started_at).getTime()
  return ms < 1000 ? '<1s' : `${(ms / 1000).toFixed(0)}s`
}
</script>

<template>
  <h1>Runs</h1>
  <p v-if="error" class="err">{{ error }}</p>
  <table v-else-if="runs.length" class="data">
    <thead>
      <tr>
        <th class="num">id</th>
        <th>repo</th>
        <th class="num">pr</th>
        <th>head</th>
        <th>status</th>
        <th>event</th>
        <th class="num" title="comments posted / findings produced — the filter working or not">posted / produced</th>
        <th class="num">cost</th>
        <th class="num">duration</th>
        <th>started</th>
      </tr>
    </thead>
    <tbody>
      <tr v-for="r in runs" :key="r.id" class="rowlink" @click="router.push(`/runs/${r.id}`)">
        <td class="num">{{ r.id }}</td>
        <td>{{ r.repo || '(local)' }}</td>
        <td class="num">{{ r.pr_number || '—' }}</td>
        <td class="mono">{{ shortSha(r.head_sha) }}</td>
        <td :class="r.status === 'completed' ? 'status-ok' : r.status === 'degraded' || r.status === 'failed' ? 'status-bad' : 'dim'">
          {{ r.status }}
        </td>
        <td class="mono dim">{{ r.review_event || '—' }}</td>
        <td class="num"><strong>{{ r.comments_posted }}</strong> <span class="faint">/ {{ r.findings_produced }}</span></td>
        <td class="num">{{ fmtCost(r.total_cost_usd) }}</td>
        <td class="num">{{ duration(r) }}</td>
        <td class="dim">{{ r.started_at }}</td>
      </tr>
    </tbody>
  </table>
  <p v-else class="empty">No runs yet. Start one with <span class="mono">revu review &lt;pr&gt;</span> or <span class="mono">revu review --local</span>.</p>
</template>
