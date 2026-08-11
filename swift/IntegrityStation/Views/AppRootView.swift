import SwiftUI

struct AppRootView: View {
    @Environment(AppController.self) private var controller

    var body: some View {
        TabView {
            StationsView()
                .tabItem {
                    Label(String(localized: "stations.title"), systemImage: "antenna.radiowaves.left.and.right")
                }

            EventsView()
                .tabItem {
                    Label(String(localized: "events.title"), systemImage: "waveform.path.ecg")
                }

            #if os(iOS)
            SettingsView()
                .tabItem {
                    Label(String(localized: "settings.title"), systemImage: "gearshape")
                }
            #endif
        }
        .tint(.accentColor)
        .preferredColorScheme(controller.settings.appearance.colorScheme)
        .task { controller.start() }
    }
}

#if os(macOS)
struct MenuBarStatusView: View {
    @Environment(AppController.self) private var controller

    var body: some View {
        TimelineView(.periodic(from: .now, by: 1)) { _ in
            VStack(alignment: .leading, spacing: 10) {
                HealthLabel(state: controller.store.rollupHealth)
                    .font(.headline)

                if controller.settings.stationIDs.isEmpty {
                    Text("menu_bar.configure")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                } else {
                    Divider()
                    ForEach(controller.settings.stationIDs, id: \.self) { stationID in
                        HStack(spacing: 8) {
                            Circle()
                                .fill(StationPalette.health(controller.store.health(for: stationID)))
                                .frame(width: 8, height: 8)
                            VStack(alignment: .leading, spacing: 1) {
                                Text(controller.settings.label(for: stationID) ?? stationID)
                                    .font(.caption.weight(.semibold))
                                Text(StationFormat.age(seconds: controller.store.currentLastSeenAge(for: stationID)))
                                    .font(.caption2.monospacedDigit())
                                    .foregroundStyle(.secondary)
                            }
                            Spacer()
                        }
                    }
                }
            }
            .padding(14)
            .frame(width: 290, alignment: .leading)
        }
    }
}
#endif
