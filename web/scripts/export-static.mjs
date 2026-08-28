import { readFile, rm, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const documentPath = path.join(webRoot, 'dist', 'index.html')
const serverBundle = path.join(webRoot, '.static-ssr', 'entry-server.js')

try {
  const [{ render }, document] = await Promise.all([
    import(pathToFileURL(serverBundle)),
    readFile(documentPath, 'utf8'),
  ])
  const appHtml = await render()
  const marker = '<!--app-html-->'
  if (!document.includes(marker)) {
    throw new Error(`Static app marker missing from ${documentPath}`)
  }
  await writeFile(documentPath, document.replace(marker, appHtml), 'utf8')
  console.log('static site exported — 1 semantic HTML page')
} finally {
  await rm(path.join(webRoot, '.static-ssr'), { recursive: true, force: true })
}
