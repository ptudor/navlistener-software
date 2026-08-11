import SwiftUI

struct OnboardingView: View {
    @Environment(AppController.self) private var controller
    @State private var serverDraft = ""
    @State private var manualStationID = ""
    @State private var validationMessage: String?

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
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
                    #if os(iOS)
                    .textInputAutocapitalization(.never)
                    .keyboardType(.URL)
                    #endif
                    .font(.body.monospaced())

                    Button {
                        connect()
                    } label: {
                        Label(String(localized: "onboarding.connect"), systemImage: "arrow.right.circle.fill")
                            .frame(maxWidth: .infinity)
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(serverDraft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }

                if controller.serverURL != nil {
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
                                        controller.addStation(id: observer.id)
                                    }
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
                        #if os(iOS)
                        .textInputAutocapitalization(.never)
                        #endif
                        Button(String(localized: "onboarding.station.add_manual")) {
                            addManualStation()
                        }
                        .buttonStyle(.bordered)
                    }
                }

                if let validationMessage {
                    FeedErrorBanner(message: validationMessage)
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
    }

    private func connect() {
        do {
            try controller.connect(to: serverDraft)
            validationMessage = nil
        } catch {
            validationMessage = error.localizedDescription
        }
    }

    private func addManualStation() {
        let id = manualStationID.trimmingCharacters(in: .whitespacesAndNewlines)
        guard AppController.isValidStationID(id) else {
            validationMessage = String(localized: "onboarding.station.invalid")
            return
        }
        controller.addStation(id: id)
        manualStationID = ""
        validationMessage = nil
    }
}
