import SwiftUI

struct BoardTelemetryView: View {
    @Environment(AppController.self) private var controller
    let board: StationBoard

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 300), alignment: .top)], spacing: 14) {
                if let sample = board.latest {
                    if let environment = sample.details?.environment {
                        InstrumentCard("board.environment.title", systemImage: "thermometer.medium") {
                            freshness(sample, stale: board.stale)
                            EnvironmentReadings(environment: environment)
                            note("board.environment.note")
                        }
                    }
                    if let resources = sample.details?.resources {
                        resourcesCard(resources, sample: sample)
                    }
                    diagnosticsCard(sample)
                }
            }
            if let sample = board.update, let update = sample.details?.update {
                updateCard(update, sample: sample)
            }
            if let sample = board.timing, let timing = sample.details?.timing {
                timingCard(timing, sample: sample)
            }
            if let event = board.lastInterference, let snapshot = event.snapshot {
                interferenceCard(event, snapshot: snapshot)
            }
        }
    }

    private func updateCard(_ update: BoardUpdate, sample: BoardSample) -> some View {
        InstrumentCard("update.title", systemImage: "arrow.down.circle") {
            freshness(sample, stale: board.updateStale, timing: true)
            MetricRow(label: "update.state", value: update.stateDescription)
            MetricRow(label: "update.mode", value: update.mode?.capitalized ?? StationFormat.unknown)
            MetricRow(label: "update.channel", value: update.channel?.capitalized ?? StationFormat.unknown)
            MetricRow(label: "update.running", value: update.runningRelease ?? StationFormat.unknown)
            MetricRow(label: "update.available", value: update.availableRelease ?? StationFormat.unknown)
            if let staged = update.stagedRelease, staged != "0" {
                MetricRow(label: "update.downloaded", value: staged)
            }
            if update.state == "downloading", let progress = update.progress { ProgressView(value: progress) }
            if let failed = update.failedRelease, failed != "0" {
                MetricRow(label: "update.failed_release", value: failed)
            }
            if let error = update.error, error != "OK" {
                Text(error).font(.caption.monospaced()).foregroundStyle(StationPalette.warning)
            }
            if update.securityFlags != 31 { note("update.development") }
        }
    }

    private func resourcesCard(_ resources: BoardResources, sample: BoardSample) -> some View {
        InstrumentCard("board.resources.title", systemImage: "memorychip") {
            freshness(sample, stale: board.stale)
            if let fraction = resources.spoolFraction {
                ProgressView(value: fraction)
                    .accessibilityLabel(Text("board.resources.used"))
            }
            MetricRow(label: "board.resources.used", value: BoardFormat.bytes(resources.spoolUsedBytes))
            MetricRow(label: "board.resources.capacity", value: BoardFormat.bytes(resources.spoolCapacityBytes))
            MetricRow(label: "board.resources.psram", value: BoardFormat.flag(resources.spoolPSRAM))
            MetricRow(label: "board.resources.queued", value: BoardFormat.count(resources.spoolRecords))
            MetricRow(label: "board.resources.dropped", value: BoardFormat.count(resources.spoolDroppedRecords))
                .foregroundStyle((resources.spoolDroppedRecords ?? 0) > 0 ? StationPalette.warning : .primary)
            MetricRow(label: "board.resources.internal_free", value: BoardFormat.bytes(resources.internalFreeBytes))
            MetricRow(label: "board.resources.psram_free", value: BoardFormat.bytes(resources.psramFreeBytes))
            note("board.resources.note")
        }
    }

    private func diagnosticsCard(_ sample: BoardSample) -> some View {
        InstrumentCard("board.diagnostics.title", systemImage: "cpu") {
            freshness(sample, stale: board.stale)
            MetricRow(label: "board.firmware", value: sample.details?.firmware ?? StationFormat.unknown, monospaced: true)
            MetricRow(label: "board.uptime", value: StationFormat.uptime(seconds: sample.details?.uptimeMS.map { Double($0) / 1_000 }))
            if let rtc = sample.details?.rtc {
                DisclosureGroup("board.rtc.title") {
                    MetricRow(label: "board.rtc.readable", value: BoardFormat.flag(rtc.hasFlag(1)))
                    MetricRow(label: "board.rtc.running", value: BoardFormat.flag(rtc.hasFlag(2)))
                    MetricRow(label: "board.rtc.calendar", value: BoardFormat.flag(rtc.hasFlag(16)))
                    MetricRow(label: "board.rtc.backup", value: BoardFormat.flag(rtc.hasFlag(4)))
                    MetricRow(label: "board.rtc.power_fail", value: BoardFormat.flag(rtc.hasFlag(8)))
                    note("board.rtc.note")
                }
            }
            if let chip = sample.details?.atecc {
                DisclosureGroup("board.atecc.title") {
                    MetricRow(label: "board.atecc.revision", value: chip.revision ?? StationFormat.unknown, monospaced: true)
                    MetricRow(label: "board.atecc.config_lock", value: BoardFormat.status(chip.configLock))
                    MetricRow(label: "board.atecc.data_lock", value: BoardFormat.status(chip.dataLock))
                    MetricRow(label: "board.atecc.rng", value: BoardFormat.status(chip.rngScreening))
                    note("board.atecc.note")
                }
            }
            if let eeprom = sample.details?.eeprom {
                DisclosureGroup("board.eeprom.title") {
                    MetricRow(label: "board.eeprom.action", value: BoardFormat.status(eeprom.action))
                    MetricRow(label: "board.eeprom.eui", value: eeprom.eui64 ?? StationFormat.unknown, monospaced: true)
                    MetricRow(label: "board.eeprom.capabilities", value: BoardFormat.flag(eeprom.capabilitiesValid))
                    if eeprom.capabilitiesValid == true {
                        MetricRow(label: "board.eeprom.revision", value: BoardFormat.count(eeprom.revision))
                        MetricRow(label: "board.eeprom.components", value: BoardFormat.count(eeprom.componentCount))
                    }
                    note("board.eeprom.note")
                }
            }
            DisclosureGroup("board.sample.title") { sampleIdentity(sample) }
        }
    }

    private func timingCard(_ timing: BoardTiming, sample: BoardSample) -> some View {
        InstrumentCard("board.timing.title", systemImage: "waveform.path") {
            freshness(sample, stale: board.timingStale, timing: true)
            MetricRow(label: "board.timing.phase", value: StationFormat.clockDrift(nanoseconds: timing.rtcMinusGNSSPhaseNS))
            MetricRow(label: "board.timing.square_wave", value: BoardFormat.status(timing.rtcSquareWaveState))
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 250), alignment: .top)], spacing: 16) {
                if let gnss = timing.gnss { channel(gnss, title: "board.timing.gnss") }
                if let rtc = timing.rtc { channel(rtc, title: "board.timing.rtc") }
            }
            DisclosureGroup("board.timing.capture") {
                MetricRow(label: "board.timing.resolution", value: timing.resolutionHz.map { "\($0) Hz" } ?? StationFormat.unknown)
                MetricRow(label: "board.timing.dropped", value: BoardFormat.count(timing.captureQueueDropped))
                MetricRow(label: "board.timing.elapsed", value: StationFormat.uptime(seconds: timing.elapsedMS.map { Double($0) / 1_000 }))
                sampleIdentity(sample)
            }
            note("board.timing.note")
        }
    }

    private func channel(_ channel: BoardTimingChannel, title: LocalizedStringKey) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(title).font(.subheadline.weight(.semibold))
            MetricRow(label: "board.timing.edge", value: channel.flags.map { _ in
                String(localized: channel.hasFlags(5) ? "board.timing.edge_fresh" : "board.timing.edge_missing")
            } ?? StationFormat.unknown)
            MetricRow(label: "board.timing.period", value: BoardFormat.measurement(channel.validPeriodNS.map { $0 / 1e9 }, unit: "s", digits: 9))
            MetricRow(label: "board.timing.width", value: BoardFormat.measurement(channel.validWidthNS.map { $0 / 1e6 }, unit: "ms", digits: 6))
            MetricRow(label: "board.timing.error", value: BoardFormat.measurement(channel.validPeriodErrorPPM, unit: "ppm", digits: 3))
            MetricRow(label: "board.timing.hardware_count", value: BoardFormat.count(channel.validHardwarePulses))
            MetricRow(label: "board.timing.captured", value: BoardFormat.count(channel.capturedRisingEdges))
            MetricRow(label: "board.timing.missing", value: BoardFormat.count(channel.missingPulseEstimate))
            MetricRow(label: "board.timing.discontinuities", value: BoardFormat.count(channel.discontinuities))
            MetricRow(label: "board.timing.counter_discontinuities", value: BoardFormat.count(channel.counterDiscontinuities))
        }
    }

    private func interferenceCard(_ event: BoardInterference, snapshot: BoardSample) -> some View {
        InstrumentCard("board.interference.title", systemImage: "antenna.radiowaves.left.and.right") {
            MetricRow(label: "board.interference.transitions", value: BoardFormat.count(snapshot.details?.eventCount))
            MetricRow(label: "board.interference.uptime", value: StationFormat.uptime(seconds: snapshot.details?.eventUptimeMS.map { Double($0) / 1_000 }))
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 250), alignment: .top)], spacing: 16) {
                if let before = event.before, before.session == snapshot.session {
                    eventEnvironment(before, title: "board.interference.before")
                }
                eventEnvironment(snapshot, title: "board.interference.snapshot")
            }
            note("board.interference.note")
        }
    }

    private func eventEnvironment(_ sample: BoardSample, title: LocalizedStringKey) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(title).font(.subheadline.weight(.semibold))
            MetricRow(label: "board.sample.utc", value: BoardFormat.timestamp(sample.sampleTime))
            MetricRow(label: "board.uptime", value: StationFormat.uptime(seconds: sample.details?.uptimeMS.map { Double($0) / 1_000 }))
            if let environment = sample.details?.environment {
                EnvironmentReadings(environment: environment)
            }
        }
    }

    private func freshness(_ sample: BoardSample, stale: Bool?, timing: Bool = false) -> some View {
        let state = controller.store.boardFreshness(sample, stale: stale, timing: timing)
        let key: LocalizedStringKey = switch state {
        case .current: "board.freshness.current"
        case .receiptOnly: "board.freshness.receipt_only"
        case .stale: "board.freshness.stale"
        case .unknown: "board.freshness.unknown"
        }
        return Label { Text(key) } icon: {
            Image(systemName: state == .current ? "clock" : "clock.badge.questionmark")
        }
        .font(.caption)
        .foregroundStyle(state == .current ? .secondary : StationPalette.warning)
    }

    private func sampleIdentity(_ sample: BoardSample) -> some View {
        VStack(spacing: 8) {
            MetricRow(label: "board.sample.received", value: BoardFormat.timestamp(sample.receivedAt))
            MetricRow(label: "board.sample.utc", value: BoardFormat.timestamp(sample.sampleTime))
            MetricRow(label: "board.sample.session", value: sample.session ?? StationFormat.unknown, monospaced: true)
            MetricRow(label: "board.sample.sequence", value: BoardFormat.count(sample.sequence))
        }
    }

    private func note(_ key: LocalizedStringKey) -> some View {
        Text(key).font(.caption).foregroundStyle(.secondary)
    }
}

private struct EnvironmentReadings: View {
    let environment: BoardEnvironment

    var body: some View {
        MetricRow(label: "board.environment.mcp", value: BoardFormat.measurement(environment.mcp9808C, unit: "°C"))
        MetricRow(label: "board.environment.hdc", value: BoardFormat.measurement(environment.hdc2080C, unit: "°C"))
        MetricRow(label: "board.environment.bmp", value: BoardFormat.measurement(environment.bmp388BMP384C, unit: "°C"))
        MetricRow(label: "board.environment.humidity", value: BoardFormat.measurement(environment.humidityPercent, unit: "% RH"))
        MetricRow(label: "board.environment.pressure", value: BoardFormat.measurement(environment.pressurePa.map { Double($0) / 100 }, unit: "hPa"))
    }
}
