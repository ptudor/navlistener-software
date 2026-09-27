import SwiftUI

struct ReceptionView: View {
    @Environment(AppController.self) private var controller
    let board: StationBoard
    let sample: BoardSample
    let assessment: ReceptionAssessment

    private var current: Bool {
        let state = controller.store.boardFreshness(sample, stale: board.receptionStale, reception: true)
        return state == .current || state == .receiptOnly
    }

    private var constellations: [Int] {
        let expected = assessment.expected ?? []
        return [0, 1, 2, 3, 5, 6, 7].filter { g in
            let bit = UInt8(1 << g)
            return (expected.count == 8 && expected[g] > 0)
                || (assessment.alarmMask ?? 0) & bit != 0
                || (sample.collectorReception?.alarmMask ?? 0) & bit != 0
        }
    }

    var body: some View {
        InstrumentCard("reception.title", systemImage: "antenna.radiowaves.left.and.right") {
            Text(LocalizedStringKey(unitKey))
                .font(.caption).foregroundStyle(.secondary)
            if !current { Text("board.freshness.stale").foregroundStyle(StationPalette.warning) }
            if constellations.isEmpty { Text("board.freshness.unknown") }
            ForEach(constellations, id: \.self) { g in
                VStack(alignment: .leading, spacing: 6) {
                    Text(ConstellationName.localized(gnssid: g)).font(.headline)
                    verdict(g, label: "reception.edge", value: assessment)
                    verdict(g, label: "reception.collector", value: sample.collectorReception)
                }
                .padding(.vertical, 4)
            }
            if (sample.collectorReception?.disagreementMask ?? 0) != 0 {
                Text("reception.disagreement").foregroundStyle(StationPalette.warning)
            }
            Text("reception.note").font(.caption).foregroundStyle(.secondary)
        }
    }

    private var unitKey: String {
        guard let perSignal = sample.collectorReception?.perSignal else { return "reception.entries" }
        return perSignal ? "reception.signals" : "reception.satellites"
    }

    private func verdict(_ gnss: Int, label: LocalizedStringKey, value: ReceptionAssessment?) -> some View {
        HStack {
            Text(label)
            Spacer()
            Text(value?.counts(gnss) ?? StationFormat.unknown).monospacedDigit()
            Text(LocalizedStringKey(value?.stateKey(gnss, current: current) ?? "board.freshness.unknown"))
                .foregroundStyle((value?.alarmMask ?? 0) & UInt8(1 << gnss) != 0 ? StationPalette.warning : .secondary)
        }
        .font(.subheadline)
    }
}
