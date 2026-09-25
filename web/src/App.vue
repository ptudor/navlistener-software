<script setup>
import { onBeforeUnmount, onMounted, ref } from 'vue'

const menuOpen = ref(false)
const menuButton = ref(null)
const header = ref(null)

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
    eyebrow: 'Receive',
    title: 'Receive satellite messages',
    copy: 'Bring a patch of sky into focus. Our purpose-built ESP32-S3 observer combines a satellite receiver, Wi-Fi, and station sensors on one board.',
    tags: ['u-blox + Septentrio', 'Open hardware'],
  },
  {
    number: '02',
    eyebrow: 'Record',
    title: 'Archive each message',
    copy: 'Feeder software sends the messages to a collector. The archive keeps each original message with the identity of the station that received it.',
    tags: ['Raw message archive', 'Station identity'],
  },
  {
    number: '03',
    eyebrow: 'Understand',
    title: 'Decode orbits and clocks',
    copy: 'Decoders turn supported broadcasts into satellite orbits, clock corrections, and health information. The calculations follow published signal specifications.',
    tags: ['Orbit and clock models', 'Documented calculations'],
  },
  {
    number: '04',
    eyebrow: 'Explore',
    title: 'Compare observations',
    copy: 'Compare observations across stations and over time. Use the API and Integrity Station app to explore the data, investigate changes, and build your own tools.',
    tags: ['Data API', 'Integrity Station app'],
  },
]

const boards = [
  { id: 'neo', title: 'NEO', size: '127.0 × 50.8 mm', height: 640 },
  { id: 'max', title: 'MAX', size: '99.06 × 99.06 mm', height: 1600 },
  { id: 'zed', title: 'ZED / X20', size: '99.06 × 99.06 mm', height: 1600 },
]

const contributionPaths = [
  {
    icon: '⌁',
    title: 'Host an observer',
    copy: 'Have room for an antenna and an internet connection? Start with our observer or a supported receiver. Tell us where you’d like to listen and we’ll help you find a setup.',
    action: 'Talk about hosting',
    subject: 'I can host a NavListen observer',
  },
  {
    icon: '⟨⟩',
    title: 'Make something with us',
    copy: 'Help with hardware, software, testing, documentation, or design. Bring a skill you enjoy using, or something you’d like to learn.',
    action: 'Find a way to contribute',
    subject: 'I want to build NavListen',
  },
  {
    icon: '✦',
    title: 'Bring your perspective',
    copy: 'A research question, a classroom project, a useful connection, or an idea we haven’t thought of. We’d like to hear what brought you here.',
    action: 'Share an idea',
    subject: 'An idea for NavListen',
  },
]

const questions = [
  {
    question: 'What do I need to host an observer?',
    answer: 'Give your observer a suitable antenna with a clear view of the sky, USB-C power, and Wi-Fi. Scan its setup label with Integrity Station on your iPhone or iPad to connect over Bluetooth and add your network and station details. Browser setup is available too. We’ll help you choose the right antenna and find a good spot.',
  },
  {
    question: 'Can I take part without hardware?',
    answer: 'Absolutely. Software, documentation, design, testing, and research all help the project grow. Tell us what interests you and we’ll find a useful place to begin.',
  },
  {
    question: 'Can my team use NavListen?',
    answer: 'We welcome conversations about research deployments, site monitoring, and integration into your own tools. Share your goals, location, and timeline so we can discuss the hardware, data access, and support that fit your project.',
  },
  {
    question: 'Can I use a receiver I already have?',
    answer: 'The stack works with u-blox and Septentrio receiver data. The messages available depend on the model and firmware: u-blox navigation messages are decoded centrally, while Septentrio data is currently captured for the archive. Send us your receiver model and we’ll help check what it can contribute.',
  },
]

function closeMenu() {
  menuOpen.value = false
}

function handleKeydown(event) {
  if (event.key === 'Escape' && menuOpen.value) {
    closeMenu()
    menuButton.value?.focus()
  }
}

function handlePointerdown(event) {
  if (menuOpen.value && !header.value?.contains(event.target)) closeMenu()
}

function handleFocusout(event) {
  if (!header.value?.contains(event.relatedTarget)) closeMenu()
}

onMounted(() => {
  document.addEventListener('keydown', handleKeydown)
  document.addEventListener('pointerdown', handlePointerdown)
})

onBeforeUnmount(() => {
  document.removeEventListener('keydown', handleKeydown)
  document.removeEventListener('pointerdown', handlePointerdown)
})

function emailHref(subject) {
  return `mailto:ptudor@ptudor.net?subject=${encodeURIComponent(subject)}`
}
</script>

<template>
  <a class="skip-link" href="#main-content">Skip to main content</a>

  <header ref="header" class="site-header" @focusout="handleFocusout">
    <div class="nav-shell">
      <a class="brand" href="#top" aria-label="NavListen home" @click="closeMenu">
        <img src="/assets/mark.svg?v=compass-1" width="38" height="38" alt="" />
        <span>navlisten</span>
      </a>

      <button
        ref="menuButton"
        class="menu-button"
        type="button"
        :aria-expanded="menuOpen"
        aria-controls="site-navigation"
        :aria-label="menuOpen ? 'Close navigation' : 'Open navigation'"
        @click="menuOpen = !menuOpen"
      >
        <span></span><span></span>
      </button>

      <nav id="site-navigation" :class="{ open: menuOpen }" aria-label="Main navigation">
        <a href="#why" @click="closeMenu">Why listen</a>
        <a href="#system" @click="closeMenu">How it works</a>
        <a href="#hardware" @click="closeMenu">Hardware</a>
        <a href="#join" @click="closeMenu">Join us</a>
        <a class="nav-cta" href="#work" @click="closeMenu">Get results with us <span aria-hidden="true">↗</span></a>
      </nav>
    </div>
  </header>

  <main id="main-content" tabindex="-1">
    <section id="top" class="hero">
      <div class="hero-grid container">
        <div class="hero-copy">
          <p class="eyebrow"><span class="pulse-dot"></span> An open navigation observatory</p>
          <h1>Satellites broadcast.<br /><em>We listen.</em></h1>
          <p class="hero-lede">
            NavListen records satellite navigation messages so we can study orbits, clocks,
            and signals. Host an observer, explore the data, or help build the tools.
          </p>
          <div class="hero-actions">
            <a class="button button-primary" href="#join">
              Get involved <span aria-hidden="true">↓</span>
            </a>
            <a class="button button-secondary" href="#system">See how it works <span aria-hidden="true">↓</span></a>
          </div>
          <p class="hero-note">Open hardware. Open software. Everyone’s welcome.</p>
        </div>

        <div class="sky-instrument" role="img" aria-label="Illustration of navigation satellites sending messages to an observer on Earth">
          <div class="instrument-label"><span>A SHARED SKY</span><span>ILLUSTRATION</span></div>
          <div class="sky-grid"></div>
          <div class="earth-limb"></div>
          <div class="orbit orbit-a"><i></i></div>
          <div class="orbit orbit-b"><i></i></div>
          <div class="orbit orbit-c"><i></i></div>
          <div class="signal-line signal-one"></div>
          <div class="signal-line signal-two"></div>
          <div class="observer-beacon"><b></b><span>OBSERVER ON EARTH</span></div>
          <div class="sat-label sat-g"><b></b><span>GPS</span></div>
          <div class="sat-label sat-e"><b></b><span>Galileo</span></div>
          <div class="sat-label sat-c"><b></b><span>BeiDou</span></div>
          <div class="sat-label sat-j"><b></b><span>QZSS</span></div>
          <div class="instrument-readout">
            <span>Satellite navigation.</span>
            <strong>Beyond the blue dot.</strong>
          </div>
        </div>
      </div>
    </section>

    <section class="constellation-strip" aria-label="Navigation systems in the project’s architecture">
      <div class="container constellation-strip-inner">
        <p>One observatory.<br /><strong>A whole sky to explore.</strong></p>
        <ul class="constellation-row">
          <li v-for="item in constellations" :key="item.code" :class="item.tone">
            <b aria-hidden="true">{{ item.code }}</b>{{ item.name }}
          </li>
        </ul>
      </div>
    </section>

    <section id="why" class="manifesto section-pad">
      <div class="container manifesto-grid">
        <div>
          <p class="section-kicker">Why listen</p>
          <h2>Track satellite orbits and clocks.</h2>
        </div>
        <div class="manifesto-copy">
          <p>
            Navigation satellites broadcast the orbit and clock data receivers use to calculate
            position and time. They also send information about satellite health and available signals.
          </p>
          <p>
            NavListen archives these messages and compares observations from different stations.
            Researchers and builders can return to the original data to investigate changes
            and develop their own tools.
          </p>
        </div>
      </div>
      <div class="container use-cases">
        <article><p class="section-kicker">For research</p><h3>Study orbits and clocks.</h3><p>Explore how satellite orbits and clocks change, with original messages to revisit.</p></article>
        <article><p class="section-kicker">For operations</p><h3>Compare receiver observations.</h3><p>Study observations from your receiver alongside data from other stations.</p></article>
        <article><p class="section-kicker">For builders</p><h3>Build with open tools.</h3><p>Use the hardware, software, and API to create tools and experiments.</p></article>
      </div>
    </section>

    <section id="system" class="system section-pad">
      <div class="container section-heading split-heading">
        <div>
          <p class="section-kicker">How it works</p>
          <h2>From antenna to evidence.</h2>
        </div>
        <p>
          Receive, archive, decode, and compare satellite messages.
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
          <p class="section-kicker">A small board. A whole sky.</p>
          <h2>Meet the observer.</h2>
          <p class="hardware-lede">
            Put your corner of the sky to work. The observer sends satellite messages and local
            conditions to NavListen over Wi-Fi. Its constellation lights bring reception to your
            desk, with brightness you can adjust at a touch.
          </p>
          <dl class="hardware-specs">
            <div><dt>Receiver</dt><dd>u-blox receiver options<br /><span>Navigation messages straight from the satellites</span></dd></div>
            <div><dt>Feeder</dt><dd>ESP32-S3<br /><span>Wi-Fi connected. USB-C powered.</span></dd></div>
            <div><dt>Identity</dt><dd>Hardware-backed station identity<br /><span>A secure element keeps the station’s key on the board</span></dd></div>
            <div><dt>Sensors</dt><dd>Pressure · temperature · humidity<br /><span>See what’s happening around your receiver</span></dd></div>
          </dl>
          <a class="text-link" :href="emailHref('I’m interested in NavListen observer hardware')">Ask about the observer <span aria-hidden="true">↗</span></a>
        </div>
      </div>

      <div class="container board-gallery">
        <div class="board-gallery-heading">
          <p class="section-kicker">Receiver options</p>
          <h3>Three boards. One observer.</h3>
          <p>
            Each board is sized for a different receiver module. These 2D layout illustrations
            come from the saved board designs, with simplified component bodies and lettering.
          </p>
        </div>
        <figure v-for="board in boards" :key="board.id">
          <div class="board-sides">
            <div v-for="side in ['top', 'bottom']" :key="side" class="board-side">
              <span aria-hidden="true">{{ side }}</span>
              <img
                :src="`/assets/boards/${board.id}-${side}.png`"
                width="1600"
                :height="board.height"
                :alt="`${board.title} board, ${side} side, 2D layout illustration`"
                loading="lazy"
                decoding="async"
              />
            </div>
          </div>
          <figcaption><strong>{{ board.title }}</strong><span>{{ board.size }} · 2D layout illustration</span></figcaption>
        </figure>
      </div>
    </section>

    <section class="station section-pad" aria-labelledby="station-heading">
      <div class="container section-heading split-heading">
        <div>
          <p class="section-kicker">Integrity Station companion app</p>
          <h2 id="station-heading">Your station. Wherever you are.</h2>
        </div>
        <p>From the first connection to your next discovery, keep your observer close.</p>
      </div>
      <div class="container use-cases">
        <article>
          <p class="section-kicker">Make the connection</p>
          <h3>Start with a scan.</h3>
          <p>Scan your observer’s setup label with your iPhone or iPad. Bluetooth setup brings
            your Wi-Fi and station details across. Your patch of sky joins the picture.</p>
        </article>
        <article>
          <p class="section-kicker">Stay in touch</p>
          <h3>Know how it’s doing.</h3>
          <p>Check reception, local conditions, and station health from your iPhone, iPad, or Mac.
            Private diagnostics help you spot interruptions and understand what changed.</p>
        </article>
        <article>
          <p class="section-kicker">Keep discovering</p>
          <h3>Let your observer grow.</h3>
          <p>Bring improvements to your observer with signed firmware updates over Wi-Fi.
            Choose when to update and check its firmware version in the app.</p>
        </article>
      </div>
    </section>

    <section class="software section-pad">
      <div class="container software-grid">
        <div class="console-window">
          <div class="console-bar">
            <span><i></i><i></i><i></i></span>
            <b>INSIDE AN OBSERVATION</b>
            <em>ILLUSTRATED GUIDE</em>
          </div>
          <div class="console-body">
            <div class="console-summary">
              <div><span>From space</span><strong>A navigation message</strong></div>
              <div><span>On the ground</span><strong>A receiving station</strong></div>
            </div>
            <div class="station-card">
              <div class="station-head">
                <span class="record-mark" aria-hidden="true">↳</span>
                <div><strong>One message, a lasting record</strong><small>What we keep, and why it matters</small></div>
              </div>
              <dl class="observation-record">
                <div><dt>Original broadcast</dt><dd>The message as received, ready to decode again.</dd></div>
                <div><dt>Time &amp; source</dt><dd>When it arrived and which station heard it.</dd></div>
                <div><dt>Decoded information</dt><dd>Orbit, clock, and health data from supported signals.</dd></div>
              </dl>
              <p class="record-note"><span aria-hidden="true">↺</span> A record you can return to as the tools improve.</p>
            </div>
          </div>
        </div>

        <div class="software-copy">
          <p class="section-kicker">A reusable message archive</p>
          <h2>Revisit every recorded message.</h2>
          <p>
            NavListen keeps original navigation messages alongside their decoded information.
            As decoders improve or new questions arise, you can run the calculations again
            using the same recorded data.
          </p>
          <ul class="check-list">
            <li><span>01</span><div><strong>Follow the calculations</strong><p>Explore decoders and orbit models written from published satellite specifications.</p></div></li>
            <li><span>02</span><div><strong>Put changes in context</strong><p>Review orbit, clock, and receiver observations alongside recorded events.</p></div></li>
            <li><span>03</span><div><strong>Work with the data</strong><p>Use the API, event stream, and Integrity Station app in your own investigations.</p></div></li>
          </ul>
        </div>
      </div>
    </section>

    <section class="coverage section-pad">
      <div class="container coverage-inner">
        <div class="coverage-copy">
          <p class="section-kicker">Expand the network</p>
          <h2>Help us observe more satellites.</h2>
          <p>
            Stations in different locations receive signals from different satellites. Their
            observations help us compare local conditions and study regional navigation systems.
            Host an observer at home, on campus, or at work to contribute data from your location.
          </p>
          <a class="text-link" href="#join">Explore hosting a station <span aria-hidden="true">→</span></a>
        </div>
        <div class="network-map" role="img" aria-label="Concept illustration of observers connecting across locations, with a place for your station">
          <span class="network-caption">A NETWORK WE CAN BUILD TOGETHER</span>
          <div class="map-grid"></div>
          <div class="network-path path-a"></div>
          <div class="network-path path-b"></div>
          <div class="network-path path-c"></div>
          <div class="node node-a"><i></i><span>A HOME</span></div>
          <div class="node node-b"><i></i><span>A CAMPUS</span></div>
          <div class="node node-c"><i></i><span>A WORKPLACE</span></div>
          <div class="node node-d"><i></i><span>A RESEARCH SITE</span></div>
          <div class="node node-e"><i></i><span>YOUR STATION</span></div>
        </div>
      </div>
    </section>

    <section id="join" class="join section-pad">
      <div class="container join-heading">
        <p class="section-kicker">Host, build, or contribute</p>
        <h2>Come help us listen.</h2>
        <p>
          This project gets better with more disciplines, more stations, and more points of view.
          You’re welcome whether satellite navigation is your day job or a new curiosity.
        </p>
      </div>

      <div class="container contribution-grid">
        <article v-for="path in contributionPaths" :key="path.title" class="contribution-card">
          <span class="contribution-icon" aria-hidden="true">{{ path.icon }}</span>
          <h3>{{ path.title }}</h3>
          <p>{{ path.copy }}</p>
          <a :href="emailHref(path.subject)">{{ path.action }} <span aria-hidden="true">↗</span></a>
        </article>
      </div>

      <div id="work" class="container project-inquiry">
        <div>
          <p class="section-kicker">For teams &amp; organizations</p>
          <h3>Tell us what you want to achieve.</h3>
          <p>Understand observations at your site, answer a research question, or put navigation data
            to use in your own tools. Let’s talk about the results you need and how NavListen can help.</p>
        </div>
        <div class="project-inquiry-action">
          <a class="button button-secondary" :href="emailHref('Using NavListen in our organization')">Discuss your goals <span aria-hidden="true">↗</span></a>
          <p>A conversation is a good place to start.</p>
        </div>
      </div>

      <div class="container getting-started">
        <div>
          <p class="section-kicker">Getting started</p>
          <h3>Plan your contribution.</h3>
          <p>Find out what you need to host an observer, use your receiver, or contribute without hardware.</p>
        </div>
        <div class="question-list">
          <details v-for="item in questions" :key="item.question">
            <summary>{{ item.question }}<span aria-hidden="true">+</span></summary>
            <p>{{ item.answer }}</p>
          </details>
        </div>
      </div>

      <div class="container final-cta">
        <div>
          <span class="pulse-dot"></span>
          <p>JOIN NAVLISTEN</p>
          <h2>Let’s get started.</h2>
          <a class="contact-address" href="mailto:ptudor@ptudor.net">ptudor@ptudor.net</a>
        </div>
        <a class="button button-light" :href="emailHref('Hello NavListen')">Say hello <span aria-hidden="true">↗</span></a>
      </div>
    </section>
  </main>

  <footer>
    <div class="container footer-grid">
      <div>
        <a class="brand footer-brand" href="#top"><img src="/assets/mark.svg?v=compass-1" width="34" height="34" alt="" /><span>navlisten</span></a>
        <p>Open tools for studying satellite navigation.</p>
      </div>
      <nav aria-label="Footer navigation">
        <a href="#why">Why listen</a>
        <a href="#system">How it works</a>
        <a href="#hardware">Hardware</a>
        <a href="#work">Get results with us</a>
        <a :href="emailHref('Hello NavListen')">Contact</a>
      </nav>
      <div class="footer-meta">
        <nav class="footer-legal" aria-label="Legal">
          <a href="https://intsat.space/intsat/terms/">Terms &amp; Conditions</a>
          <a href="https://intsat.space/intsat/privacy/">Privacy Policy</a>
        </nav>
        <span>© 2026 NavListen</span>
      </div>
    </div>
  </footer>
</template>
