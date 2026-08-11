import SwiftUI

struct SkyPlotView: View {
    @Environment(\.colorScheme) private var colorScheme
    let signals: [String: StationSignal]

    private var markers: [Marker] {
        signals.compactMap { key, signal in
            guard let azimuth = signal.aziDeg,
                  let elevation = signal.elevDeg,
                  let point = SkyPlot.project(azimuthDegrees: azimuth, elevationDegrees: elevation)
            else { return nil }
            return Marker(
                id: key,
                label: signal.name ?? key,
                gnssid: signal.gnssid,
                used: signal.used,
                point: point
            )
        }
        .sorted { lhs, rhs in
            if lhs.gnssid != rhs.gnssid { return (lhs.gnssid ?? Int.max) < (rhs.gnssid ?? Int.max) }
            return lhs.id < rhs.id
        }
    }

    var body: some View {
        GeometryReader { geometry in
            let side = min(geometry.size.width, geometry.size.height)
            let radius = max(0, side / 2 - 20)
            let center = CGPoint(x: geometry.size.width / 2, y: geometry.size.height / 2)
            ZStack {
                Canvas { context, _ in
                    drawGrid(context: &context, center: center, radius: radius)
                }
                cardinalLabels(center: center, radius: radius)
                ForEach(markers) { marker in
                    markerView(marker)
                        .position(
                            x: center.x + radius * marker.point.x,
                            y: center.y + radius * marker.point.y
                        )
                }
                if markers.isEmpty {
                    Text("station.sky.not_reported")
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .position(center)
                }
            }
        }
        .aspectRatio(1, contentMode: .fit)
        .frame(minHeight: 250)
        .accessibilityElement(children: .contain)
        .accessibilityLabel(String(format: String(localized: "station.sky.accessibility"), markers.count))
    }

    private func drawGrid(context: inout GraphicsContext, center: CGPoint, radius: CGFloat) {
        for elevation in [0, 30, 60] {
            let ringRadius = CGFloat(90 - elevation) / 90 * radius
            let rect = CGRect(
                x: center.x - ringRadius,
                y: center.y - ringRadius,
                width: ringRadius * 2,
                height: ringRadius * 2
            )
            context.stroke(
                Path(ellipseIn: rect),
                with: .color(.secondary.opacity(elevation == 0 ? 0.55 : 0.28)),
                lineWidth: elevation == 0 ? 1.2 : 0.7
            )
        }
        var crosshair = Path()
        crosshair.move(to: CGPoint(x: center.x, y: center.y - radius))
        crosshair.addLine(to: CGPoint(x: center.x, y: center.y + radius))
        crosshair.move(to: CGPoint(x: center.x - radius, y: center.y))
        crosshair.addLine(to: CGPoint(x: center.x + radius, y: center.y))
        context.stroke(crosshair, with: .color(.secondary.opacity(0.18)), lineWidth: 0.7)
    }

    private func cardinalLabels(center: CGPoint, radius: CGFloat) -> some View {
        let offset = radius + 11
        let labels: [(String, CGFloat, CGFloat)] = [
            (String(localized: "cardinal.north"), 0, -offset),
            (String(localized: "cardinal.east"), offset, 0),
            (String(localized: "cardinal.south"), 0, offset),
            (String(localized: "cardinal.west"), -offset, 0),
        ]
        return ForEach(labels, id: \.0) { label, dx, dy in
            Text(label)
                .font(.caption2.weight(.bold))
                .foregroundStyle(.secondary)
                .position(x: center.x + dx, y: center.y + dy)
        }
    }

    private func markerView(_ marker: Marker) -> some View {
        let color = StationPalette.constellation(gnssid: marker.gnssid ?? -1, scheme: colorScheme)
        return ZStack {
            Circle().fill(marker.used == true ? color : color.opacity(0.12))
            Circle().strokeBorder(color, lineWidth: marker.used == true ? 1 : 1.5)
            Text(marker.label)
                .font(.system(size: 8, weight: .bold, design: .monospaced))
                .foregroundStyle(marker.used == true ? Color.black.opacity(0.82) : color)
                .lineLimit(1)
                .minimumScaleFactor(0.6)
        }
        .frame(width: 23, height: 23)
        .accessibilityLabel(marker.label)
        .accessibilityValue(marker.used == true ? String(localized: "station.sky.used") : String(localized: "station.sky.tracked"))
    }
}

private struct Marker: Identifiable {
    let id: String
    let label: String
    let gnssid: Int?
    let used: Bool?
    let point: SkyPlotPoint
}
