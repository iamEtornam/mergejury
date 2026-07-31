<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { subscribe, type RunEvent } from '../api'

const events = ref<RunEvent[]>([])
const connected = ref(false)
let cleanup: (() => void) | null = null

onMounted(() => {
  cleanup = subscribe(null, (e) => {
    events.value.unshift(e)
    if (events.value.length > 500) events.value.pop()
  })
  connected.value = true
})
onUnmounted(() => cleanup?.())

function fmtPayload(p: unknown): string {
  if (p == null) return ''
  if (typeof p === 'string') return p
  return Object.entries(p as Record<string, unknown>)
    .map(([k, v]) => `${k}=${JSON.stringify(v)}`)
    .join('  ')
}
</script>

<template>
  <h1>Live</h1>
  <p class="dim">
    Events stream here as runs progress: adapters as they finish, then clusters, then verdicts.
    A fan-out where one adapter is slow is visible here and nowhere else.
  </p>
  <table class="data" v-if="events.length">
    <thead>
      <tr>
        <th>time</th>
        <th class="num">run</th>
        <th>event</th>
        <th>detail</th>
      </tr>
    </thead>
    <tbody>
      <tr v-for="(e, i) in events" :key="events.length - i" class="event-row">
        <td class="mono dim">{{ new Date(e.at).toLocaleTimeString() }}</td>
        <td class="num">
          <router-link v-if="e.run_id" :to="`/runs/${e.run_id}`">{{ e.run_id }}</router-link>
          <span v-else>—</span>
        </td>
        <td class="mono">{{ e.type }}</td>
        <td class="mono dim">{{ fmtPayload(e.payload) }}</td>
      </tr>
    </tbody>
  </table>
  <p v-else class="empty">Listening… start a run and its events appear here.</p>
</template>
