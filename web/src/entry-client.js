import { createApp, createSSRApp } from 'vue'
import App from './App.vue'
import './styles.css'

document.documentElement.classList.replace('no-js', 'js')
// Development serves the source shell; production serves pre-rendered HTML.
const app = import.meta.env.DEV ? createApp(App) : createSSRApp(App)
app.mount('#app')
