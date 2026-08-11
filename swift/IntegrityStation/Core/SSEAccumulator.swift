import Foundation

struct SSEEventFrame: Equatable, Sendable {
    let id: String?
    let event: String
    let data: String
}

/// Pure line accumulator for docs/OUTPUT.md §3's named gnss/status/resolved
/// events. Transport and JSON decoding live in Services/EventStream.swift.
struct SSEAccumulator: Sendable {
    private(set) var lastEventID: String?
    private var currentID: String?
    private var eventName: String?
    private var dataLines: [String] = []
    private var isFirstLine = true

    mutating func consume(_ incomingLine: String) -> SSEEventFrame? {
        var line = incomingLine
        if line.last == "\r" { line.removeLast() }
        if isFirstLine {
            line = String(line.drop(while: { $0 == "\u{feff}" }))
            isFirstLine = false
        }

        if line.isEmpty {
            return dispatch()
        }
        guard line.first != ":" else { return nil }

        let field: Substring
        var value: Substring
        if let colon = line.firstIndex(of: ":") {
            field = line[..<colon]
            value = line[line.index(after: colon)...]
            if value.first == " " { value = value.dropFirst() }
        } else {
            field = Substring(line)
            value = ""
        }

        switch field {
        case "data":
            dataLines.append(String(value))
        case "event":
            eventName = String(value)
        case "id" where !value.contains("\0"):
            currentID = String(value)
        default:
            break
        }
        return nil
    }

    private mutating func dispatch() -> SSEEventFrame? {
        defer {
            currentID = nil
            eventName = nil
            dataLines.removeAll(keepingCapacity: true)
        }
        guard !dataLines.isEmpty else { return nil }
        if let currentID { lastEventID = currentID }
        return SSEEventFrame(
            id: currentID,
            event: eventName.flatMap { $0.isEmpty ? nil : $0 } ?? "message",
            data: dataLines.joined(separator: "\n")
        )
    }
}
