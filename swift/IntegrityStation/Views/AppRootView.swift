import SwiftUI

struct AppRootView: View {
    var body: some View {
        TabView {
            ContentUnavailableView(
                String(localized: "stations.empty.title"),
                systemImage: "antenna.radiowaves.left.and.right",
                description: Text("stations.empty.description")
            )
            .tabItem {
                Label(String(localized: "stations.title"), systemImage: "antenna.radiowaves.left.and.right")
            }

            ContentUnavailableView(
                String(localized: "events.empty.title"),
                systemImage: "waveform.path.ecg",
                description: Text("events.empty.description")
            )
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
    }
}

struct SettingsView: View {
    var body: some View {
        Form {
            Section(String(localized: "settings.server.section")) {
                LabeledContent(String(localized: "settings.server.url"), value: "https://collector.invalid")
            }

            Section(String(localized: "settings.about.section")) {
                LabeledContent(String(localized: "settings.about.name"), value: "Integrity Station")
                LabeledContent(String(localized: "settings.about.version"), value: "0.1")
            }
        }
        .formStyle(.grouped)
        .navigationTitle(Text("settings.title"))
    }
}

#if os(macOS)
struct MenuBarStatusView: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Label(String(localized: "menu_bar.unknown"), systemImage: "circle.dotted")
                .font(.headline)
            Text("menu_bar.configure")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .padding(14)
        .frame(width: 280, alignment: .leading)
    }
}
#endif
