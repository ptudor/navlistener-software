import Foundation

struct UpdateAccess: Codable, Sendable {
    struct Record: Codable, Sendable {
        struct Command: Codable, Sendable {
            let commandID: String
            enum CodingKeys: String, CodingKey { case commandID = "command_id" }
        }
        let command: Command
        let status: BoardUpdate?
    }
    struct Choice: Codable, Sendable {
        let generation: String
        let release: String
        let advisory: String
    }
    let requestStatus: String
    let record: Record
    let choice: Choice?
    enum CodingKeys: String, CodingKey {
        case requestStatus = "request_status"
        case record, choice
    }
}
