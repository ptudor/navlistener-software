// Offline STEP scene rendering. Only the resulting images and manifest are published.
import { createHash } from 'node:crypto'
import { createServer } from 'node:http'
import { createRequire } from 'node:module'
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { parseArgs } from 'node:util'

const { values } = parseArgs({ options: {
  scenes: { type: 'string' }, output: { type: 'string' }, tools: { type: 'string' },
  browser: { type: 'string' },
} })
for (const key of ['scenes', 'output', 'tools']) {
  if (!values[key]) throw Error(`--${key} is required`)
}
const require = createRequire(path.resolve(values.tools, 'package.json'))
const { chromium } = require('playwright')
const threeRoot = path.dirname(path.dirname(require.resolve('three')))
const manifest = JSON.parse(await readFile(path.join(values.scenes, 'manifest.json'), 'utf8'))
const html = `<!doctype html><html><head><style>
html, body { margin: 0; overflow: hidden; background: transparent; } canvas { display: block; }
</style><script type="importmap">{"imports":{"three":"/three/build/three.module.js"}}</script></head>
<body><script type="module">
import * as THREE from 'three';
const models = await fetch('/models.json').then(r => r.json());
const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true, preserveDrawingBuffer: true });
renderer.setSize(${manifest.width_pixels}, ${manifest.height_pixels});
renderer.setPixelRatio(1);
renderer.shadowMap.enabled = true;
renderer.shadowMap.type = THREE.PCFSoftShadowMap;
renderer.outputColorSpace = THREE.SRGBColorSpace;
renderer.toneMapping = THREE.ACESFilmicToneMapping;
renderer.toneMappingExposure = 1.05;
document.body.appendChild(renderer.domElement);
const cache = new Map();
function mesh(data) {
  if (!cache.has(data)) {
    const geometry = new THREE.BufferGeometry();
    geometry.setAttribute('position', new THREE.Float32BufferAttribute(data.vertices, 3));
    geometry.setIndex(data.faces);
    geometry.computeVertexNormals();
    const colour = new THREE.Color().setRGB(...data.colour, THREE.SRGBColorSpace);
    const [r,g,b] = data.colour;
    const neutral = Math.max(r,g,b) - Math.min(r,g,b) < 0.13;
    const material = new THREE.MeshStandardMaterial({ color: colour, side: THREE.DoubleSide,
      roughness: neutral && r > 0.5 ? 0.36 : 0.6, metalness: neutral && r > 0.5 ? 0.35 : 0.08,
      flatShading: true });
    cache.set(data, [geometry, material]);
  }
  const result = new THREE.Mesh(...cache.get(data));
  result.castShadow = result.receiveShadow = true;
  return result;
}
window.renderBoard = async (id, bottom) => {
  const data = await fetch('/' + id + '.json').then(r => r.json());
  const scene = new THREE.Scene();
  const assembly = new THREE.Group();
  for (const part of data.board) assembly.add(mesh(part));
  for (const instance of data.instances) {
    const component = new THREE.Group();
    for (const part of models[instance.model]) component.add(mesh(part));
    component.rotation.set(0, instance.bottom ? Math.PI : 0, instance.angle * Math.PI / 180, 'ZYX');
    component.position.set(instance.x, instance.y, (instance.bottom ? -1 : 1) * data.thickness / 2);
    assembly.add(component);
  }
  const bounds = new THREE.Box3().setFromObject(assembly);
  const centre = bounds.getCenter(new THREE.Vector3());
  assembly.position.sub(centre);
  scene.add(assembly);
  const direction = new THREE.Vector3(bottom ? -0.28 : 0.28, -0.62, bottom ? -1.25 : 1.25).normalize();
  const camera = new THREE.OrthographicCamera(-100,100,75,-75,1,1000);
  camera.position.copy(direction.clone().multiplyScalar(300));
  camera.up.set(0,1,0);
  camera.lookAt(0,0,0);
  camera.updateMatrixWorld();
  const view = new THREE.Box3();
  for (const x of [bounds.min.x,bounds.max.x]) for (const y of [bounds.min.y,bounds.max.y])
    for (const z of [bounds.min.z,bounds.max.z])
      view.expandByPoint(new THREE.Vector3(x,y,z).sub(centre).applyMatrix4(camera.matrixWorldInverse));
  const aspect = ${manifest.width_pixels / manifest.height_pixels};
  const halfHeight = Math.max((view.max.y-view.min.y)/2, (view.max.x-view.min.x)/2/aspect)*1.1;
  camera.left = -halfHeight*aspect; camera.right = halfHeight*aspect;
  camera.top = halfHeight; camera.bottom = -halfHeight;
  camera.updateProjectionMatrix();
  scene.add(new THREE.AmbientLight(0xffffff,1.1));
  for (const [position,intensity,shadow] of [
    [[-70,90,bottom ? -150 : 150],2.4,true], [[100,-50,bottom ? -80 : 80],1.2,false],
  ]) {
    const light = new THREE.DirectionalLight(0xffffff,intensity);
    light.position.set(...position);
    light.castShadow = shadow;
    light.shadow.mapSize.set(4096,4096);
    Object.assign(light.shadow.camera, { left:-100,right:100,top:100,bottom:-100,near:1,far:400 });
    light.shadow.normalBias = 0.06;
    light.shadow.bias = -0.00005;
    scene.add(light);
  }
  renderer.render(scene,camera);
  await new Promise(resolve => requestAnimationFrame(resolve));
};
window.ready = true;
</script></body></html>`

const server = createServer(async (req, res) => {
  try {
    if (req.url === '/') {
      res.setHeader('Content-Type', 'text/html'); res.end(html); return
    }
    const url = new URL(req.url, 'http://localhost')
    const root = url.pathname.startsWith('/three/') ? threeRoot : values.scenes
    const relative = url.pathname.startsWith('/three/') ? url.pathname.slice(7) : url.pathname.slice(1)
    const file = path.resolve(root, relative)
    if (!file.startsWith(path.resolve(root) + path.sep)) throw Error('Invalid path')
    res.setHeader('Content-Type', file.endsWith('.js') ? 'text/javascript' : 'application/json')
    res.end(await readFile(file))
  } catch { res.statusCode = 404; res.end() }
})
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
let browser
try {
  browser = await chromium.launch({ headless: true, executablePath: values.browser,
    args: ['--use-angle=swiftshader', '--enable-unsafe-swiftshader'] })
  const page = await browser.newPage({ viewport: { width: manifest.width_pixels, height: manifest.height_pixels } })
  page.on('pageerror', error => console.error(error))
  await page.goto(`http://127.0.0.1:${server.address().port}`)
  await page.waitForFunction(() => window.ready, null, { timeout: 120000 })
  await mkdir(values.output, { recursive: true })
  for (const board of manifest.boards) {
    for (const side of ['top', 'bottom']) {
      await page.evaluate(([id, bottom]) => window.renderBoard(id, bottom), [board.id, side === 'bottom'])
      const file = `${board.id}-${side}-3d.png`
      const bytes = await page.screenshot({ path: path.join(values.output, file), omitBackground: true })
      manifest.files[file] = createHash('sha256').update(bytes).digest('hex')
      console.log(`${file}: ${bytes.length} bytes`)
    }
  }
  await writeFile(path.join(values.output, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n')
} finally {
  await browser?.close()
  await new Promise(resolve => server.close(resolve))
}
