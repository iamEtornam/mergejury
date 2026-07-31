<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, fmtCost, fmtDuration, type AdapterStats, type ProbeResult } from '../api'

const stats = ref<AdapterStats[]>([])
const probes = ref<ProbeResult[]>([])
const error = ref('')

onMounted(async () => {
  try {
    stats.value = await api.stats()
    probes.value = await api.probe()
  } catch (e) {
    error.value = String(e)
  }
})

function precision(s: AdapterStats): string {
  const judged = s.resolved + s.dismissed
  if (judged === 0) return '—'
  return `${((s.resolved / judged) * 100).toFixed(0)}%`
}
</script>

<template>
  <h1>Scoreboard</h1>
  <p class="dim">
    Cost per published comment is the number that decides which adapters survive.
    Precision comes from outcomes (resolved vs dismissed) and needs a few hundred comments to mean anything.
  </p>
  <p v-if="error" class="err">{{ error }}</p>
  <table class="data" v-if="stats.length">
    <thead>
      <tr>
        <th>adapter</th>
        <th>lens</th>
        <th class="num">runs</th>
        <th class="num">produced</th>
        <th class="num">kept</th>
        <th class="num">published</th>
        <th class="num">resolved</th>
        <th class="num">dismissed</th>
        <th class="num">precision</th>
        <th class="num">median latency</th>
        <th class="num">cost</th>
        <th class="num">$ / published</th>
      </tr>
    </thead>
    <tbody>
      <tr v-for="s in stats" :key="s.adapter_id + s.lens">
        <td>{{ s.adapter_id }}</td>
        <td class="dim">{{ s.lens }}</td>
        <td class="num">{{ s.runs }}</td>
        <td class="num">{{ s.findings_produced }}</td>
        <td class="num">{{ s.findings_kept }}</td>
        <td class="num"><strong>{{ s.published }}</strong></td>
        <td class="num">{{ s.resolved }}</td>
        <td class="num">{{ s.dismissed }}</td>
        <td class="num">{{ precision(s) }}</td>
        <td class="num">{{ fmtDuration(s.median_latency_ms) }}</td>
        <td class="num">{{ fmtCost(s.total_cost_usd) }}</td>
        <td class="num"><strong>{{ s.published > 0 ? fmtCost(s.cost_per_published) : '—' }}</strong></td>
      </tr>
    </tbody>
  </table>
  <p v-else class="empty">No adapter runs recorded yet.</p>

  <h2>ADAPTER HEALTH</h2>
  <table class="data" v-if="probes.length">
    <thead>
      <tr><th>adapter</th><th>status</th><th>detail</th></tr>
    </thead>
    <tbody>
      <tr v-for="p in probes" :key="p.adapter_id">
        <td>{{ p.adapter_id }}</td>
        <td :class="p.ok ? 'status-ok' : 'status-bad'">{{ p.ok ? 'ok' : 'fail' }}</td>
        <td class="dim">{{ p.detail }}<template v-if="p.remediation"> → {{ p.remediation }}</template></td>
      </tr>
    </tbody>
  </table>
</template>
