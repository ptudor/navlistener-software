import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { execFile } from 'node:child_process'
import { cp, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { promisify } from 'node:util'
import test from 'node:test'

const execute = promisify(execFile)

test('parallel deploy digests the finished build and removes stale sidecars', async () => {
  const root = await mkdtemp(join(tmpdir(), 'navlistener-deploy-test-'))
  try {
    const bin = join(root, 'bin')
    const destination = join(root, 'published')
    await mkdir(bin)
    await mkdir(join(root, 'dist', 'assets'), { recursive: true })
    await mkdir(destination)
    await cp(new URL('../Makefile', import.meta.url), join(root, 'Makefile'))
    await writeFile(join(root, 'dist', 'index.html'), 'old page')
    await writeFile(join(root, 'dist', 'index.html.sha256'), 'old checksum')
    await writeFile(join(root, 'dist', 'removed.js'), 'old asset')
    await writeFile(join(root, 'dist', 'removed.js.sha256'), 'old checksum')
    await writeFile(join(destination, 'removed.js'), 'previous deployment')
    await writeFile(join(destination, 'removed.js.sha256'), 'previous checksum')
    await writeFile(join(bin, 'npm'), `#!/usr/bin/env node
const fs = require('node:fs/promises');
(async () => {
  if (process.argv.slice(2).join(' ') === 'test') return;
  if (process.argv.slice(2).join(' ') !== 'run build') throw Error('unexpected npm command');
  await fs.appendFile('build-count', 'build\\n');
  await fs.unlink('dist/removed.js');
  await fs.writeFile('dist/index.html', 'unfinished');
  await new Promise(resolve => setTimeout(resolve, 300));
  await fs.writeFile('dist/index.html', 'final page');
  await fs.writeFile('dist/assets/new file.js', 'final asset');
})().catch(error => { console.error(error); process.exitCode = 1; });
`, { mode: 0o755 })
    // The stub accepts only the test's private destination, checks the actual
    // deploy flags, and mirrors --delete locally. It cannot reach the real site.
    await writeFile(join(bin, 'rsync'), `#!/usr/bin/env node
const fs = require('node:fs/promises');
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
(async () => {
  assert.deepEqual(process.argv.slice(2), [
    '-a', '--delete', '--delay-updates',
    '--exclude=/assets/boards/*.png', '--exclude=/assets/boards/*.png.sha256',
    'dist/', process.env.TEST_DESTINATION + '/',
  ]);
  for (const file of ['index.html', 'assets/new file.js']) {
    const digest = crypto.createHash('sha256').update(await fs.readFile('dist/' + file)).digest('hex');
    assert.equal((await fs.readFile('dist/' + file + '.sha256', 'utf8')).slice(0, 64), digest);
  }
  await fs.rm(process.env.TEST_DESTINATION, { recursive: true });
  await fs.cp('dist', process.env.TEST_DESTINATION, { recursive: true });
})().catch(error => { console.error(error); process.exitCode = 1; });
`, { mode: 0o755 })

    await execute('make', ['-j8', 'deploy', `DEPLOY_DIR=${destination}`], {
      cwd: root,
      env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, TEST_DESTINATION: destination },
    })
    assert.equal(await readFile(join(root, 'build-count'), 'utf8'), 'build\n')
    for (const directory of [join(root, 'dist'), destination]) {
      const files = (await readdir(directory, { recursive: true, withFileTypes: true }))
        .filter(entry => entry.isFile())
      assert.equal(files.length, 4)
      for (const file of ['index.html', 'assets/new file.js']) {
        const digest = createHash('sha256').update(await readFile(join(directory, file))).digest('hex')
        assert.equal((await readFile(join(directory, `${file}.sha256`), 'utf8')).slice(0, 64), digest)
      }
      await assert.rejects(readFile(join(directory, 'removed.js')), { code: 'ENOENT' })
      await assert.rejects(readFile(join(directory, 'removed.js.sha256')), { code: 'ENOENT' })
    }
  } finally {
    await rm(root, { recursive: true, force: true })
  }
})

test('board previews are optimized before they are pushed to the web host', async () => {
  const root = await mkdtemp(join(tmpdir(), 'navlistener-boards-test-'))
  try {
    const bin = join(root, 'bin')
    const boards = ['max-top.png', 'neo-bottom.png', 'neo-top.png']
    await mkdir(bin)
    await mkdir(join(root, 'public', 'assets', 'boards'), { recursive: true })
    await cp(new URL('../Makefile', import.meta.url), join(root, 'Makefile'))
    for (const board of boards) await writeFile(join(root, 'public', 'assets', 'boards', board), 'png')
    await writeFile(join(bin, 'oxipng'), `#!/usr/bin/env node
const fs = require('node:fs');
fs.appendFileSync('commands', JSON.stringify(['oxipng', ...process.argv.slice(2)]) + '\\n');
`, { mode: 0o755 })
    // Records the transfer; it never opens a connection.
    await writeFile(join(bin, 'rsync'), `#!/usr/bin/env node
const fs = require('node:fs');
fs.appendFileSync('commands', JSON.stringify(['rsync', ...process.argv.slice(2)]) + '\\n');
`, { mode: 0o755 })

    await execute('make', ['publish-boards'], {
      cwd: root,
      env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
    })
    const sources = boards.map(board => `public/assets/boards/${board}`)
    const commands = (await readFile(join(root, 'commands'), 'utf8')).trim().split('\n').map(line => JSON.parse(line))
    assert.deepEqual(commands, [
      ['oxipng', '-o', 'max', '--strip', 'safe', ...sources],
      ['rsync', '-a', '--delay-updates', ...sources, 'junia:/usr/local/www/navlistener/web/assets/boards/'],
    ])
  } finally {
    await rm(root, { recursive: true, force: true })
  }
})
