import SwiftUI

/// The collector's station assurance assessment (docs/OUTPUT.md §1.3). Confirmed
/// changes arrive separately as `station_assurance` and `spoofing_suspected`
/// conditions and drive station health; this card shows the evidence as the
/// collector currently evaluates it, before the detector confirms a change.
struct StationAssuranceView: View {
    let assessment: StationAssessment

    var body: some View {
        InstrumentCard("station.assurance.title", systemImage: "checkmark.shield") {
            VStack(alignment: .leading, spacing: 10) {
                AssuranceStateLabel(word: assessment.state)
                    .font(.subheadline.weight(.semibold))
                if assessment.spoofingIndicated == true {
                    Label("station.assurance.spoofing_indicated", systemImage: "exclamationmark.octagon.fill")
                        .font(.callout.weight(.semibold))
                        .foregroundStyle(StationPalette.critical)
                }
                MetricRow(label: "station.assurance.score", value: StationFormat.assuranceScore(assessment.score))
                MetricRow(label: "station.assurance.unassured_domains", value: unassuredDomains, monospaced: true)
                MetricRow(label: "station.assurance.installation", value: installation)
                ForEach(Array((assessment.checks ?? []).enumerated()), id: \.offset) { _, check in
                    Divider()
                    AssuranceCheckRow(check: check)
                }
                Divider()
                Text(configuration)
                    .font(.caption2.monospaced())
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
        }
    }

    private var unassuredDomains: String {
        guard let domains = assessment.unassuredDomains, !domains.isEmpty else {
            return String(localized: "station.assurance.none")
        }
        return domains.joined(separator: ", ")
    }

    private var installation: String {
        switch assessment.mode {
        case nil, "":
            return String(localized: "station.assurance.mode.unconfigured")
        case "fixed":
            return String(localized: assessment.surveyedPosition == true
                ? "station.assurance.mode.fixed_surveyed" : "station.assurance.mode.fixed")
        case "mobile":
            guard let speed = assessment.maxSpeedMps, speed.isFinite, speed > 0 else {
                return String(localized: "station.assurance.mode.mobile")
            }
            return String(format: String(localized: "station.assurance.mode.mobile_speed"),
                          speed.formatted(.number.precision(.fractionLength(0...1))))
        case let mode?:
            return mode
        }
    }

    private var configuration: String {
        let hash = StationFormat.configurationHash(assessment.configHash)
        let engine = assessment.engineVersion.map(String.init) ?? StationFormat.unknown
        return String(format: String(localized: "station.assurance.configuration"), engine, hash)
    }
}

private struct AssuranceStateLabel: View {
    let word: String?

    var body: some View {
        let level = word.flatMap(AssuranceLevel.init(rawValue:))
        Label(AssuranceLevel.displayName(word), systemImage: level?.systemImage ?? "questionmark.circle")
            .foregroundStyle(StationPalette.assurance(level))
    }
}

private struct AssuranceCheckRow: View {
    let check: AssuranceCheck

    var body: some View {
        let level = check.state.flatMap(AssuranceLevel.init(rawValue:))
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline) {
                Circle()
                    .fill(StationPalette.assurance(level))
                    .frame(width: 7, height: 7)
                Text(check.check ?? StationFormat.unknown)
                    .font(.callout.monospaced())
                Spacer(minLength: 12)
                Text(AssuranceLevel.displayName(check.state))
                    .font(.callout)
                    .foregroundStyle(StationPalette.assurance(level))
            }
            if let domain = check.domain {
                Text(domain)
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
            }
            if check.recoveringSince != nil {
                Text(String(format: String(localized: "station.assurance.recovering"),
                            AssuranceLevel.displayName(check.candidate)))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            if let reasons = check.reasons, !reasons.isEmpty {
                Text(reasons.joined(separator: ", "))
                    .font(.caption.monospaced())
                    .foregroundStyle(.secondary)
            }
            if let values = StationFormat.assuranceValues(check.metrics) {
                Text(String(format: String(localized: "station.assurance.measured"), values))
                    .font(.caption2.monospaced())
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
            if let limits = StationFormat.assuranceValues(check.thresholds) {
                Text(String(format: String(localized: "station.assurance.limits"), limits))
                    .font(.caption2.monospaced())
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
            }
        }
        .accessibilityElement(children: .combine)
    }
}
