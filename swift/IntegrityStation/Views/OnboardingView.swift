import SwiftUI

struct OnboardingView: View {
    @Environment(AppController.self) private var controller
    let selectionSession: ReadSession?
    init(selectionSession: ReadSession? = nil) { self.selectionSession = selectionSession }
    @State private var serverDraft = ""
    @State private var tokenDraft = ""
    @State private var manualStationID = ""
    @State private var validationMessage: String?
    @State private var showingSetup = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                if selectionSession == nil {
                VStack(alignment: .leading, spacing: 6) {
                    Image(systemName: "scope")
                        .font(.system(size: 34, weight: .medium))
                        .foregroundStyle(.tint)
                    Text("onboarding.title")
                        .font(.largeTitle.bold())
                    Text("onboarding.description")
                        .foregroundStyle(.secondary)
                }

                InstrumentCard("onboarding.server.title", systemImage: "network") {
                    TextField(
                        String(localized: "settings.server.url"),
                        text: $serverDraft,
                        prompt: Text("server.placeholder")
                    )
                    .textFieldStyle(.roundedBorder)
                    .accessibilityIdentifier("collector.url")
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    .keyboardType(.URL)
                    #endif
                    .font(.body.monospaced())

                    SecureField(
                        String(localized: "settings.server.read_token"),
                        text: $tokenDraft
                    )
                    .textFieldStyle(.roundedBorder)
                    .font(.body.monospaced())
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    #endif
                    if controller.hasStoredCredential && tokenDraft.isEmpty {
                        Text("settings.server.credential_saved")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    } else {
                        Text("settings.server.credential_note")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }

                    Button {
                        Task { await connect() }
                    } label: {
                        Label(String(localized: "onboarding.connect"), systemImage: "arrow.right.circle.fill")
                            .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                    .accessibilityIdentifier("collector.connect")
                    .disabled(
                        serverDraft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
                            || controller.isConnecting
                    )

                    if controller.isConnecting {
                        ProgressView()
                            .frame(maxWidth: .infinity)
                    }
                }

                }
                #if os(iOS)
                InstrumentCard("setup.title", systemImage: "sensor.tag.radiowaves.forward") {
                    Button("setup.open") { showingSetup = true }
                        .accessibilityIdentifier("setup.open")
                }
                #endif
                if controller.serverURL != nil {
                    if selectionSession == nil { audienceCard }

                    InstrumentCard("onboarding.station.title", systemImage: "antenna.radiowaves.left.and.right") {
                        if controller.store.observers.isEmpty {
                            Text("onboarding.station.no_discovery")
                                .font(.callout)
                                .foregroundStyle(.secondary)
                        } else {
                            ForEach(controller.store.observers) { observer in
                                HStack {
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(observer.remark ?? observer.id).font(.callout.weight(.semibold))
                                        Text(observer.id).font(.caption.monospaced()).foregroundStyle(.secondary)
                                    }
                                    Spacer()
                                    Button(String(localized: "action.add")) {
                                        do {
                                            try controller.addStation(id: observer.id, for: selectionSession)
                                            validationMessage = nil
                                        } catch { validationMessage = error.localizedDescription }
                                    }
                                    .accessibilityIdentifier("station.discovered.\(observer.id)")
                                    .disabled(controller.settings.stationIDs.contains(observer.id))
                                }
                            }
                        }

                        Divider()
                        TextField(
                            String(localized: "onboarding.station.manual"),
                            text: $manualStationID,
                            prompt: Text("onboarding.station.placeholder")
                        )
                        .textFieldStyle(.roundedBorder)
                        .font(.body.monospaced())
                        .accessibilityIdentifier("station.manual")
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                        Button(String(localized: "onboarding.station.add_manual")) {
                            addManualStation()
                        }
                        .buttonStyle(.bordered)
                        .accessibilityIdentifier("station.addManual")
                    }
                }

                if let validationMessage {
                    FeedErrorBanner(message: validationMessage)
                } else if let error = controller.connectionError {
                    FeedErrorBanner(message: error)
                } else if let error = controller.store.errorMessage {
                    FeedErrorBanner(message: error)
                }
            }
            .padding(20)
            .frame(maxWidth: 660)
            .frame(maxWidth: .infinity)
        }
        .task {
            if serverDraft.isEmpty { serverDraft = controller.settings.serverURLString }
        }
        #if os(iOS)
        .sheet(isPresented: $showingSetup) { ObserverSetupView() }
        #endif
    }

    @ViewBuilder
    private var audienceCard: some View {
        InstrumentCard("settings.audience.section", systemImage: "person.2.badge.key") {
            Picker(
                String(localized: "settings.audience.selection"),
                selection: Binding(
                    get: { controller.selectedAudience },
                    set: { audience in Task { await select(audience) } }
                )
            ) {
                ForEach(controller.availableAudiences) { audience in
                    Text(audience.rawValue).tag(audience)
                }
            }
            .pickerStyle(.menu)

            LabeledContent(
                String(localized: "settings.audience.principal"),
                value: controller.principalID
            )
            .font(.caption.monospaced())
            Text("settings.audience.note")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    private func connect() async {
        do {
            let enteredToken = tokenDraft.trimmingCharacters(in: .whitespacesAndNewlines)
            try await controller.connect(
                to: serverDraft,
                readToken: enteredToken.isEmpty ? nil : enteredToken
            )
            tokenDraft = ""
            validationMessage = nil
        } catch {
            validationMessage = error.localizedDescription
        }
    }

    private func select(_ audience: ReadAudience) async {
        do {
            try await controller.selectAudience(audience)
            validationMessage = nil
        } catch {
            validationMessage = error.localizedDescription
        }
    }

    private func addManualStation() {
        let id = manualStationID
        guard AppController.isValidStationID(id) else {
            validationMessage = String(localized: "onboarding.station.invalid")
            return
        }
        do {
            try controller.addStation(id: id, for: selectionSession)
            manualStationID = ""
            validationMessage = nil
        } catch { validationMessage = error.localizedDescription }
    }
}
