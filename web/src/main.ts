import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import RunsList from './views/RunsList.vue'
import RunDetail from './views/RunDetail.vue'
import LiveRun from './views/LiveRun.vue'
import Scoreboard from './views/Scoreboard.vue'
import Prompts from './views/Prompts.vue'
import './style.css'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: RunsList },
    { path: '/runs/:id', component: RunDetail },
    { path: '/live', component: LiveRun },
    { path: '/scoreboard', component: Scoreboard },
    { path: '/prompts', component: Prompts },
  ],
})

createApp(App).use(router).mount('#app')
