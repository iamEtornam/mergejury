<script setup lang="ts">
</script>

<template>
  <header class="shell">
    <router-link class="mark" to="/">revu</router-link>
    <nav>
      <router-link to="/">runs</router-link>
      <router-link to="/live">live</router-link>
      <router-link to="/scoreboard">scoreboard</router-link>
      <router-link to="/prompts">prompts</router-link>
    </nav>
  </header>
  <main class="content">
    <router-view />
  </main>
</template>
