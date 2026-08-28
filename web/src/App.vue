<script setup>
import { ref } from 'vue'

const menuOpen = ref(false)

const constellations = [
  { name: 'GPS', code: 'G', tone: 'gps' },
  { name: 'SBAS', code: 'S', tone: 'sbas' },
  { name: 'Galileo', code: 'E', tone: 'galileo' },
  { name: 'BeiDou', code: 'C', tone: 'beidou' },
  { name: 'QZSS', code: 'J', tone: 'qzss' },
  { name: 'GLONASS', code: 'R', tone: 'glonass' },
  { name: 'NavIC', code: 'I', tone: 'navic' },
]

const layers = [
  {
    number: '01',
    eyebrow: 'The observer',
    title: 'Listen at the edge',
    copy: 'Our purpose-built ESP32 observer pairs a u-blox or Septentrio receiver with hardware-backed ECC identity, then forwards the raw navigation messages satellites actually broadcast.',
    tags: ['u-blox + Septentrio', 'Hardware ECC identity', 'Environmental sensors'],
  },
  {
    number: '02',
    eyebrow: 'The wire',
    title: 'Keep every frame',
    copy: 'Tiny C and ESP32 feeders reconnect, retransmit, and preserve provenance. The edge stays simple so every decoder improvement can replay history.',
    tags: ['GNF1 over TLS', 'Disk spool', 'Signed observations'],
  },
  {
    number: '03',
    eyebrow: 'The core',
    title: 'Turn signals into evidence',
    copy: 'A clean-room Go library decodes public signal specifications, propagates every orbit, and checks clocks, ephemerides, and receiver physics.',
    tags: ['ICD-authored math', 'Integrity events', 'Timescale history'],
  },
  {
    number: '04',
    eyebrow: 'The network',
    title: 'Compare what the sky told us',
    copy: 'Independent stations corroborate the same broadcast from different places. Federation makes a solo receiver part of something much larger.',
    tags: ['Open API', 'Live event stream', 'Federated trust'],
  },
]

const contributionPaths = [
  {
    icon: '⌁',
    title: 'Put a station under your sky',
    copy: 'Run a feeder with a supported receiver or help bring up the fabrication-ready observer board. Geographic diversity is the instrument.',
    action: 'I can host an observer',
    subject: 'I can host a NavListen observer',
  },
  {
    icon: '⟨⟩',
    title: 'Build the open stack',
    copy: 'Work on GNSS decoders, Go services, embedded C, Swift clients, RF detection, data visualization, or the public API.',
    action: 'I want to build',
    subject: 'I want to build NavListen',
  },
  {
    icon: '✦',
    title: 'Bring your perspective',
    copy: 'Navigation needs RF engineers, researchers, technical writers, fabricators, operators, and curious people who ask better questions.',
    action: 'I have another idea',
    subject: 'An idea for NavListen',
  },
]

function closeMenu() {
  menuOpen.value = false
}

function emailHref(subject) {
  return `mailto:ptudor@ptudor.net?subject=${encodeURIComponent(subject)}`
}
</script>

<template>
  <a class="skip-link" href="#main-content">Skip to main content</a>

  <header class="site-header">
    <div class="nav-shell">
      <a class="brand" href="#top" aria-label="NavListen home" @click="closeMenu">
        <img src="/assets/mark.svg" width="38" height="38" alt="" />
        <span>navlisten</span>
      </a>

      <button
        class="menu-button"
        type="button"
        :aria-expanded="menuOpen"
        aria-controls="site-navigation"
        aria-label="Toggle navigation"
        @click="menuOpen = !menuOpen"
      >
        <span></span><span></span>
      </button>

      <nav id="site-navigation" :class="{ open: menuOpen }" aria-label="Main navigation">
        <a href="#why" @click="closeMenu">Why listen</a>
        <a href="#system" @click="closeMenu">The system</a>
        <a href="#hardware" @click="closeMenu">Hardware</a>
        <a href="#join" @click="closeMenu">Join us</a>
        <a class="nav-cta" :href="emailHref('I want to join NavListen')">Get involved <span>↗</span></a>
      </nav>
    </div>
  </header>

  <main id="main-content" tabindex="-1">
    <section id="top" class="hero">
      <div class="hero-grid container">
        <div class="hero-copy">
          <p class="eyebrow"><span class="pulse-dot"></span> An open navigation observatory</p>
          <h1>Navigation is a broadcast.<br /><em>We’re listening.</em></h1>
          <p class="hero-lede">
            NavListen is open hardware and software for recording, decoding, and independently
            checking the satellite signals that tell the world where—and when—it is.
          </p>
          <div class="hero-actions">
            <a class="button button-primary" :href="emailHref('I want to join NavListen')">
              Join the project <span>↗</span>
            </a>
            <a class="button button-secondary" href="#system">See how it works <span>↓</span></a>
          </div>
          <p class="hero-note">Open source · Apache 2.0 · Built in public</p>
        </div>

        <div class="sky-instrument" aria-label="Illustration of live satellite signals reaching an observer">
          <div class="instrument-label"><span>SKY / LIVE</span><span>37.7749° N</span></div>
          <div class="sky-grid"></div>
          <div class="earth-limb"></div>
          <div class="orbit orbit-a"><i></i></div>
          <div class="orbit orbit-b"><i></i></div>
          <div class="orbit orbit-c"><i></i></div>
          <div class="signal-line signal-one"></div>
          <div class="signal-line signal-two"></div>
          <div class="observer-beacon"><b></b><span>OBSERVER 01</span></div>
          <div class="sat-label sat-g"><b>G</b><span>12</span></div>
          <div class="sat-label sat-e"><b>E</b><span>19</span></div>
          <div class="sat-label sat-c"><b>C</b><span>08</span></div>
          <div class="sat-label sat-j"><b>J</b><span>03</span></div>
          <div class="instrument-readout">
            <span><i class="status-ok"></i> frame stream</span>
            <strong>0 anomalies</strong>
          </div>
        </div>
      </div>

      <div class="signal-ticker" aria-hidden="true">
        <div>
          <span>RXM-SFRBX</span><b>G12</b><i>1010100110010110</i><span>EPHEMERIS CURRENT</span><b>E19</b><i>0011010101001011</i><span>PPS +0.3 ns</span><b>J03</b><i>1100101101001001</i>
          <span>RXM-SFRBX</span><b>G12</b><i>1010100110010110</i><span>EPHEMERIS CURRENT</span><b>E19</b><i>0011010101001011</i><span>PPS +0.3 ns</span><b>J03</b><i>1100101101001001</i>
        </div>
      </div>
    </section>

    <section class="proof-strip" aria-label="Project principles">
      <div class="container proof-grid">
        <div><strong>7</strong><span>civil constellations<br />in one architecture</span></div>
        <div><strong>RAW</strong><span>broadcast frames<br />kept as evidence</span></div>
        <div><strong>OPEN</strong><span>hardware, software,<br />math, and protocol</span></div>
        <div><strong>OURS</strong><span>independent stations,<br />shared understanding</span></div>
      </div>
    </section>

    <section id="why" class="manifesto section-pad">
      <div class="container manifesto-grid">
        <div>
          <p class="section-kicker">Why this exists</p>
          <h2>Coordinates deserve an independent witness.</h2>
        </div>
        <div class="manifesto-copy">
          <p>
            Phones, ships, power grids, data centers, farms, and emergency services all take
            timing and position from radio signals sent thousands of kilometers through space.
            Interference is local. Bad ephemerides are global. Either can look ordinary if no one
            is keeping the original broadcast.
          </p>
          <p>
            We think navigation deserves a public observatory: independent receivers, transparent
            math, replayable history, and enough geographic reach to tell a broken satellite from
            a broken sky over one neighborhood.
          </p>
        </div>
      </div>
      <div class="container statement">
        <p>Trusting a blue dot is easy.</p>
        <p><em>Proving it is the work.</em></p>
      </div>
    </section>

    <section id="system" class="system section-pad">
      <div class="container section-heading split-heading">
        <div>
          <p class="section-kicker">A complete signal path</p>
          <h2>From antenna to evidence.</h2>
        </div>
        <p>
          Each layer has a narrow job, a documented interface, and an inspectable record.
        </p>
      </div>

      <div class="container layer-list">
        <article v-for="layer in layers" :key="layer.number" class="layer-card">
          <span class="layer-number">{{ layer.number }}</span>
          <div class="layer-title">
            <p>{{ layer.eyebrow }}</p>
            <h3>{{ layer.title }}</h3>
          </div>
          <p class="layer-copy">{{ layer.copy }}</p>
          <ul>
            <li v-for="tag in layer.tags" :key="tag">{{ tag }}</li>
          </ul>
        </article>
      </div>
    </section>

    <section id="hardware" class="hardware section-pad">
      <div class="container hardware-grid">
        <div class="board-stage">
          <div class="board-meta board-meta-top"><span>NAVLISTEN OBSERVER</span><span>REV A</span></div>
          <img
            src="/assets/observer-board.svg"
            width="1000"
            height="500"
            alt="Illustrated layout of the NavListen GNSS observer board"
            loading="lazy"
          />
          <div class="board-meta board-meta-bottom"><span>127.0 × 50.8 MM</span><span>4 LAYER</span></div>
        </div>

        <div class="hardware-copy">
          <p class="section-kicker">Open hardware, with a pulse</p>
          <h2>Meet the observer.</h2>
          <p class="hardware-lede">
            A fabrication-ready four-layer board built specifically to observe navigation. The
            receiver listens. The sensors add physical context. The secure element lets every
            observation keep its identity across the network.
          </p>
          <dl class="hardware-specs">
            <div><dt>Receiver</dt><dd>u-blox or Septentrio<br /><span>single- and dual-band options</span></dd></div>
            <div><dt>Edge</dt><dd>ESP32<br /><span>Wi-Fi, USB-C, resilient feeder</span></dd></div>
            <div><dt>Identity</dt><dd>ATECC608 + EUI-64<br /><span>Non-extractable station key</span></dd></div>
            <div><dt>Senses</dt><dd>Pressure · temperature · humidity<br /><span>Context for physical integrity gates</span></dd></div>
          </dl>
          <a class="text-link" href="#join">Help build the first network <span>→</span></a>
        </div>
      </div>
    </section>

    <section class="software section-pad">
      <div class="container software-grid">
        <div class="console-window">
          <div class="console-bar">
            <span><i></i><i></i><i></i></span>
            <b>integrity station / live</b>
            <em>PUBLIC</em>
          </div>
          <div class="console-body">
            <div class="console-summary">
              <div><span>Network health</span><strong><i></i> Nominal</strong></div>
              <div><span>Last frame</span><strong>0.4 s ago</strong></div>
            </div>
            <div class="station-card">
              <div class="station-head">
                <span class="station-pulse"></span>
                <div><strong>Coastal observer</strong><small>02:4f:91:ff:fe:70:2a:11</small></div>
                <time>UP 18D</time>
              </div>
              <div class="constellation-row">
                <span v-for="item in constellations" :key="item.code" :class="item.tone">
                  <b>{{ item.code }}</b>{{ item.name }}
                </span>
              </div>
              <div class="sparkline" aria-hidden="true">
                <svg viewBox="0 0 640 92" role="img">
                  <path class="spark-grid" d="M0 23H640M0 46H640M0 69H640" />
                  <path class="spark-fill" d="M0 65 L35 61 L70 63 L105 45 L140 49 L175 43 L210 46 L245 31 L280 34 L315 26 L350 32 L385 28 L420 38 L455 29 L490 35 L525 20 L560 25 L595 19 L640 24 L640 92 L0 92 Z" />
                  <path class="spark-line" d="M0 65 L35 61 L70 63 L105 45 L140 49 L175 43 L210 46 L245 31 L280 34 L315 26 L350 32 L385 28 L420 38 L455 29 L490 35 L525 20 L560 25 L595 19 L640 24" />
                </svg>
              </div>
              <div class="event-row"><span><i></i> Orbit agreement</span><strong>≤ 0.42 m</strong><small>across 4 observers</small></div>
            </div>
          </div>
        </div>

        <div class="software-copy">
          <p class="section-kicker">Software that remembers</p>
          <h2>Ask what the sky said.</h2>
          <p>
            NavListen stores raw navigation frames beside decoded state. The archive can be replayed
            when a decoder is corrected or a new signal is added. When two receivers disagree, the
            original observation is still there.
          </p>
          <ul class="check-list">
            <li><span>01</span><div><strong>Clean-room GNSS math</strong><p>Go decoders and orbit models authored from public interface specifications.</p></div></li>
            <li><span>02</span><div><strong>Live integrity detection</strong><p>Orbit, clock, RF, and physical plausibility become typed, replayable events.</p></div></li>
            <li><span>03</span><div><strong>Tools for people</strong><p>A versioned API, event stream, and native Integrity Station clients for the network.</p></div></li>
          </ul>
        </div>
      </div>
    </section>

    <section class="coverage section-pad">
      <div class="container coverage-inner">
        <div class="coverage-copy">
          <p class="section-kicker">One sky, many witnesses</p>
          <h2>A useful network starts with the next rooftop.</h2>
          <p>
            One station can preserve a broadcast. A fleet can distinguish receiver trouble from
            local interference. A federation can see regional systems from the places they were built to serve.
          </p>
        </div>
        <div class="network-map" aria-label="Illustration of a federated observer network">
          <div class="map-grid"></div>
          <div class="network-path path-a"></div>
          <div class="network-path path-b"></div>
          <div class="network-path path-c"></div>
          <div class="node node-a"><i></i><span>CALIFORNIA</span></div>
          <div class="node node-b"><i></i><span>JAPAN</span></div>
          <div class="node node-c"><i></i><span>INDIA</span></div>
          <div class="node node-d"><i></i><span>EUROPE</span></div>
          <div class="node node-e"><i></i><span>YOU?</span></div>
        </div>
      </div>
    </section>

    <section id="join" class="join section-pad">
      <div class="container join-heading">
        <p class="section-kicker">There is room at the workbench</p>
        <h2>Come help us listen.</h2>
        <p>
          This project gets better with more disciplines, more stations, and more points of view.
          Curiosity is enough to begin; GNSS expertise can come later.
        </p>
      </div>

      <div class="container contribution-grid">
        <article v-for="path in contributionPaths" :key="path.title" class="contribution-card">
          <span class="contribution-icon">{{ path.icon }}</span>
          <h3>{{ path.title }}</h3>
          <p>{{ path.copy }}</p>
          <a :href="emailHref(path.subject)">{{ path.action }} <span>↗</span></a>
        </article>
      </div>

      <div class="container final-cta">
        <div>
          <span class="pulse-dot"></span>
          <p>THE SKY IS ALREADY TALKING</p>
          <h2>Let’s hear it together.</h2>
        </div>
        <a class="button button-light" :href="emailHref('I want to join NavListen')">Join NavListen <span>↗</span></a>
      </div>
    </section>
  </main>

  <footer>
    <div class="container footer-grid">
      <div>
        <a class="brand footer-brand" href="#top"><img src="/assets/mark.svg" width="34" height="34" alt="" /><span>navlisten</span></a>
        <p>Open tools for understanding the signals above us.</p>
      </div>
      <nav aria-label="Footer navigation">
        <a href="#why">Why listen</a>
        <a href="#system">The system</a>
        <a href="#hardware">Hardware</a>
        <a :href="emailHref('Hello NavListen')">Contact</a>
      </nav>
      <div class="footer-meta">
        <span>Apache License 2.0</span>
        <span>© 2026 NavListen</span>
      </div>
    </div>
  </footer>
</template>
