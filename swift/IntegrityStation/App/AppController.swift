import Foundation
import Observation

@MainActor
@Observable
final class AppController {
    let store: StationStore
    let settings: AppSettings

    init(store: StationStore = StationStore(), settings: AppSettings = AppSettings()) {
        self.store = store
        self.settings = settings
    }

    var serverURL: URL? {
        Self.validServerURL(settings.serverURLString)
    }

    func start() {
        guard let serverURL else { return }
        store.start(baseURL: serverURL, stationIDs: settings.stationIDs)
    }

    func connect(to input: String) throws {
        guard let url = Self.validServerURL(input) else { throw FeedError.invalidBaseURL }
        settings.serverURLString = url.absoluteString
        store.start(baseURL: url, stationIDs: settings.stationIDs)
    }

    func refresh() async {
        guard let serverURL else { return }
        await store.refresh(baseURL: serverURL)
    }

    func addStation(id: String) {
        let id = id.trimmingCharacters(in: .whitespacesAndNewlines)
        guard Self.isValidStationID(id), !settings.stationIDs.contains(id) else { return }
        settings.stationIDs.append(id)
        store.selectedStationIDs = settings.stationIDs
    }

    func removeStation(id: String) {
        settings.stationIDs.removeAll { $0 == id }
        settings.labelsByStationID.removeValue(forKey: id)
        store.selectedStationIDs = settings.stationIDs
    }

    func setLabel(_ label: String, for stationID: String) {
        let trimmed = label.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { settings.labelsByStationID.removeValue(forKey: stationID) }
        else { settings.labelsByStationID[stationID] = trimmed }
    }

    static func isValidStationID(_ value: String) -> Bool {
        !value.isEmpty && value.count <= 256 && value.allSatisfy {
            $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "." || $0 == "-")
        }
    }

    static func validServerURL(_ value: String) -> URL? {
        guard let url = URL(string: value.trimmingCharacters(in: .whitespacesAndNewlines)),
              let scheme = url.scheme?.lowercased(),
              scheme == "http" || scheme == "https",
              url.host != nil
        else { return nil }
        return url
    }
}
