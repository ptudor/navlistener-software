import { createSSRApp } from 'vue'
import { renderToString } from '@vue/server-renderer'
import App from './App.vue'

export async function render() {
  return renderToString(createSSRApp(App))
}
