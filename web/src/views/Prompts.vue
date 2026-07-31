<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'

const names = ref<string[]>([])
const selected = ref('')
const content = ref('')
const committed = ref('')
const saveState = ref<'idle' | 'saving' | 'saved' | 'error'>('idle')
const saveError = ref('')

onMounted(async () => {
  names.value = (await api.prompts()).sort()
  if (names.value.length) load(names.value[0])
})

async function load(name: string) {
  selected.value = name
  saveState.value = 'idle'
  const p = await api.prompt(name)
  content.value = p.content
  committed.value = p.committed
}

const dirty = computed(() => content.value !== committed.value)

async function save() {
  saveState.value = 'saving'
  const res = await api.savePrompt(selected.value, content.value)
  if (res.ok) {
    saveState.value = 'saved'
  } else {
    saveState.value = 'error'
    saveError.value = await res.text()
  }
}
</script>

<template>
  <h1>Prompts</h1>
  <p class="dim">
    Edits write to the configured prompts dir on disk so they stay in git; this is an editor, not a store.
    Iterate against a past run with <span class="mono">revu runs replay &lt;id&gt;</span> or the replay button on a run.
  </p>
  <div style="display: flex; gap: 16px; align-items: flex-start">
    <div style="min-width: 180px">
      <div
        v-for="n in names"
        :key="n"
        class="card selectable"
        :class="{ linked: n === selected }"
        @click="load(n)"
      >
        <span class="mono">{{ n }}</span>
      </div>
    </div>
    <div style="flex: 1" v-if="selected">
      <textarea v-model="content" rows="28" spellcheck="false"></textarea>
      <div style="display: flex; gap: 10px; margin-top: 8px; align-items: center">
        <button class="primary" @click="save" :disabled="saveState === 'saving'">save to disk</button>
        <span v-if="dirty" class="dim">differs from committed version</span>
        <span v-else class="faint">matches committed version</span>
        <span v-if="saveState === 'saved'" class="status-ok">saved</span>
        <span v-if="saveState === 'error'" class="err">{{ saveError }}</span>
      </div>
    </div>
  </div>
</template>
