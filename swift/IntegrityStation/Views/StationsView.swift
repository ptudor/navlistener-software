import SwiftUI

struct StationsView: View {
    @Environment(AppController.self) private var controller

    var body: some View {
        NavigationStack {
            ZStack {
                InstrumentBackground()
                if controller.settings.stationIDs.isEmpty {
                    OnboardingView()
                } else {
                    stationList
                }
            }
            .navigationTitle(Text("stations.title"))
            .toolbar {
                ToolbarItem(placement: .primaryAction) {
                    Button {
                        Task { await controller.refresh() }
                    } label: {
                        Label(String(localized: "action.refresh"), systemImage: "arrow.clockwise")
                    }
                    .disabled(controller.store.isRefreshing || controller.serverURL == nil)
                }
            }
            .navigationDestination(for: String.self) { stationID in
                StationDetailView(stationID: stationID)
            }
        }
    }

    private var stationList: some View {
        TimelineView(.periodic(from: .now, by: 1)) { _ in
            ScrollView {
                LazyVStack(spacing: 14) {
                    if let error = controller.store.errorMessage {
                        FeedErrorBanner(message: error)
                    }
                    if controller.store.isShowingCachedSnapshot {
                        Label(String(localized: "stations.cached"), systemImage: "clock.arrow.circlepath")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }

                    ForEach(controller.settings.stationIDs, id: \.self) { stationID in
                        NavigationLink(value: stationID) {
                            StationCardView(
                                stationID: stationID,
                                label: controller.settings.label(for: stationID),
                                observer: controller.store.observers.first { $0.id == stationID },
                                health: controller.store.health(for: stationID),
                                lastSeenAge: controller.store.currentLastSeenAge(for: stationID)
                            )
                        }
                        .buttonStyle(.plain)
                    }
                }
                .padding(16)
                .frame(maxWidth: 920)
                .frame(maxWidth: .infinity)
            }
            .refreshable { await controller.refresh() }
        }
    }
}

private struct StationCardView: View {
    let stationID: String
    let label: String?
    let observer: Observer?
    let health: HealthState
    let lastSeenAge: TimeInterval?

    private var constellationCounts: [(id: Int, count: Int)] {
        let counts = (observer?.svs ?? [:]).values.reduce(into: [Int: Int]()) { result, signal in
            if let gnssid = signal.gnssid { result[gnssid, default: 0] += 1 }
        }
        return counts.map { ($0.key, $0.value) }.sorted { $0.id < $1.id }
    }

    var body: some View {
        InstrumentCard {
            HStack(alignment: .top, spacing: 14) {
                Circle()
                    .fill(StationPalette.health(health))
                    .frame(width: 12, height: 12)
                    .padding(.top, 5)
                    .shadow(color: StationPalette.health(health).opacity(0.45), radius: 5)

                VStack(alignment: .leading, spacing: 5) {
                    Text(label ?? observer?.remark ?? stationID)
                        .font(.headline)
                        .foregroundStyle(.primary)
                    Text(stationID)
                        .font(.caption.monospaced())
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                    HealthLabel(state: health)
                }
                Spacer()
                VStack(alignment: .trailing, spacing: 4) {
                    Text(StationFormat.age(seconds: lastSeenAge))
                        .font(.caption.monospacedDigit())
                    Text(StationFormat.uptime(seconds: observer?.uptimeS))
                        .font(.caption.monospacedDigit())
                        .foregroundStyle(.secondary)
                }
                Image(systemName: "chevron.right")
                    .font(.caption.weight(.bold))
                    .foregroundStyle(.tertiary)
                    .padding(.top, 5)
            }

            if constellationCounts.isEmpty {
                Text("stations.signals.not_reported")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } else {
                FlowLayout(spacing: 7) {
                    ForEach(constellationCounts, id: \.id) { row in
                        ConstellationBadge(gnssid: row.id, count: row.count)
                    }
                }
            }
        }
        .opacity(health == .offline ? 0.62 : 1)
    }
}

private struct FlowLayout: Layout {
    let spacing: CGFloat

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        arrange(proposal: proposal, subviews: subviews).size
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        let arrangement = arrange(proposal: ProposedViewSize(width: bounds.width, height: proposal.height), subviews: subviews)
        for (index, point) in arrangement.points.enumerated() {
            subviews[index].place(at: CGPoint(x: bounds.minX + point.x, y: bounds.minY + point.y), proposal: .unspecified)
        }
    }

    private func arrange(proposal: ProposedViewSize, subviews: Subviews) -> (size: CGSize, points: [CGPoint]) {
        let width = proposal.width ?? .infinity
        var x: CGFloat = 0
        var y: CGFloat = 0
        var lineHeight: CGFloat = 0
        var points: [CGPoint] = []
        for subview in subviews {
            let size = subview.sizeThatFits(.unspecified)
            if x > 0 && x + size.width > width {
                x = 0
                y += lineHeight + spacing
                lineHeight = 0
            }
            points.append(CGPoint(x: x, y: y))
            x += size.width + spacing
            lineHeight = max(lineHeight, size.height)
        }
        return (CGSize(width: min(width, max(0, x - spacing)), height: y + lineHeight), points)
    }
}
