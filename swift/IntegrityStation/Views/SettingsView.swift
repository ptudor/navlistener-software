import SwiftUI

struct SettingsView: View {
    @Environment(AppController.self) private var controller
    @State private var serverDraft = ""
    @State private var serverError: String?

    var body: some View {
        #if os(macOS)
        TabView {
            stationSettings
                .tabItem { Label(String(localized: "settings.stations.section"), systemImage: "antenna.radiowaves.left.and.right") }
            serverSettings
                .tabItem { Label(String(localized: "settings.server.section"), systemImage: "network") }
            notificationSettings
                .tabItem { Label(String(localized: "settings.notifications.section"), systemImage: "bell") }
            aboutSettings
                .tabItem { Label(String(localized: "settings.about.section"), systemImage: "info.circle") }
        }
        .frame(width: 620, height: 470)
        .padding(12)
        .task { loadDraft() }
        #else
        NavigationStack {
            Form {
                stationSection
                serverSection
                notificationSection
                appearanceSection
                aboutSection
            }
            .formStyle(.grouped)
            .navigationTitle(Text("settings.title"))
        }
        .task { loadDraft() }
        #endif
    }

    #if os(macOS)
    private var stationSettings: some View {
        Form { stationSection }
            .formStyle(.grouped)
            .padding()
    }

    private var serverSettings: some View {
        Form { serverSection }
            .formStyle(.grouped)
            .padding()
    }

    private var notificationSettings: some View {
        Form {
            notificationSection
            appearanceSection
        }
        .formStyle(.grouped)
        .padding()
    }

    private var aboutSettings: some View {
        Form { aboutSection }
            .formStyle(.grouped)
            .padding()
    }
    #endif

    @ViewBuilder
    private var stationSection: some View {
        Section(String(localized: "settings.stations.section")) {
            if controller.settings.stationIDs.isEmpty {
                Text("settings.stations.empty")
                    .foregroundStyle(.secondary)
            } else {
                ForEach(controller.settings.stationIDs, id: \.self) { stationID in
                    VStack(alignment: .leading, spacing: 5) {
                        Text(stationID)
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                        TextField(
                            String(localized: "settings.stations.label"),
                            text: Binding(
                                get: { controller.settings.label(for: stationID) ?? "" },
                                set: { controller.setLabel($0, for: stationID) }
                            )
                        )
                        .textFieldStyle(.roundedBorder)
                        Button(String(localized: "action.remove"), role: .destructive) {
                            controller.removeStation(id: stationID)
                        }
                        .buttonStyle(.borderless)
                    }
                    .padding(.vertical, 3)
                }
            }
        }
    }

    @ViewBuilder
    private var serverSection: some View {
        Section(String(localized: "settings.server.section")) {
            TextField(
                String(localized: "settings.server.url"),
                text: $serverDraft,
                prompt: Text("server.placeholder")
            )
            .font(.body.monospaced())
            #if os(iOS)
            .textInputAutocapitalization(.never)
            .keyboardType(.URL)
            #endif
            if let serverError {
                Text(serverError).font(.caption).foregroundStyle(StationPalette.warning)
            }
            Button(String(localized: "action.save")) { saveServer() }
                .disabled(serverDraft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
        }
    }

    @ViewBuilder
    private var notificationSection: some View {
        @Bindable var settings = controller.settings
        Section(String(localized: "settings.notifications.section")) {
            Toggle(String(localized: "settings.notifications.offline"), isOn: $settings.notifyOffline)
            Toggle(String(localized: "settings.notifications.rf"), isOn: $settings.notifyRF)
            Toggle(String(localized: "settings.notifications.critical"), isOn: $settings.notifyCritical)
            Text("settings.notifications.note")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    @ViewBuilder
    private var appearanceSection: some View {
        @Bindable var settings = controller.settings
        Section(String(localized: "settings.appearance.section")) {
            Picker(String(localized: "settings.appearance.mode"), selection: $settings.appearance) {
                Text("settings.appearance.system").tag(AppAppearance.system)
                Text("settings.appearance.dark").tag(AppAppearance.dark)
                Text("settings.appearance.light").tag(AppAppearance.light)
            }
        }
    }

    @ViewBuilder
    private var aboutSection: some View {
        Section(String(localized: "settings.about.section")) {
            LabeledContent(String(localized: "settings.about.name"), value: String(localized: "app.name"))
            LabeledContent(String(localized: "settings.about.version"), value: appVersion)
            Text("settings.about.description")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    private var appVersion: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "0.1"
    }

    private func loadDraft() {
        if serverDraft.isEmpty { serverDraft = controller.settings.serverURLString }
    }

    private func saveServer() {
        do {
            try controller.connect(to: serverDraft)
            serverError = nil
        } catch {
            serverError = error.localizedDescription
        }
    }
}
