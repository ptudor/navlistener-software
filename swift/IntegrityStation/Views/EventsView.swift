import SwiftUI

struct EventsView: View {
    @Environment(AppController.self) private var controller

    private var stationEvents: [GNSSAPIEvent] {
        let selected = Set(controller.settings.stationIDs)
        return controller.store.events.filter { event in
            event.stationID.map { selected.contains($0) } ?? false
        }
    }

    var body: some View {
        NavigationStack {
            ZStack {
                InstrumentBackground()
                if stationEvents.isEmpty {
                    ContentUnavailableView(
                        String(localized: "events.empty.title"),
                        systemImage: "waveform.path.ecg",
                        description: Text("events.empty.description")
                    )
                } else {
                    ScrollView {
                        LazyVStack(spacing: 10) {
                            if let message = controller.store.eventStreamMessage,
                               !controller.store.isEventStreamConnected {
                                FeedErrorBanner(message: message)
                            }
                            ForEach(Array(stationEvents.enumerated()), id: \.offset) { _, event in
                                InstrumentCard { EventRowView(event: event, showsStation: true) }
                            }
                        }
                        .padding(16)
                        .frame(maxWidth: 920)
                        .frame(maxWidth: .infinity)
                    }
                    .refreshable { await controller.refresh() }
                }
            }
            .navigationTitle(Text("events.title"))
        }
    }
}

struct EventRowView: View {
    let event: GNSSAPIEvent
    let showsStation: Bool

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            Circle()
                .fill(StationPalette.severity(event.severity))
                .frame(width: 9, height: 9)
                .padding(.top, 5)
            VStack(alignment: .leading, spacing: 4) {
                Text(event.message ?? event.type ?? String(localized: "event.unknown"))
                    .font(.callout.weight(.semibold))
                if showsStation, let stationID = event.stationID {
                    Text(stationID)
                        .font(.caption.monospaced())
                        .foregroundStyle(.secondary)
                }
                HStack(spacing: 7) {
                    if let type = event.type {
                        Text(type)
                            .font(.caption2.monospaced())
                    }
                    if let date = WireDate.parse(event.time) {
                        Text(date, style: .relative)
                            .font(.caption2.monospacedDigit())
                    }
                }
                .foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
        }
        .accessibilityElement(children: .combine)
    }
}
