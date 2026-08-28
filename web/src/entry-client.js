import { createSSRApp } from 'vue'
import App from './App.vue'
import './styles.css'

document.documentElement.classList.replace('no-js', 'js')
createSSRApp(App).mount('#app')
