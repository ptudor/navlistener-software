import Foundation

struct SkyPlotPoint: Equatable, Sendable {
    let x: Double
    let y: Double
}

enum SkyPlot {
    /// Projects served azimuth/elevation onto a unit polar plot: north is up,
    /// azimuth increases clockwise, the horizon is radius 1, and zenith is 0.
    /// Uses numeric constellation identifiers; no SV-name parsing is involved.
    static func project(azimuthDegrees: Double, elevationDegrees: Double) -> SkyPlotPoint? {
        guard azimuthDegrees.isFinite,
              elevationDegrees.isFinite,
              (0...90).contains(elevationDegrees)
        else { return nil }

        let radius = (90 - elevationDegrees) / 90
        let angle = azimuthDegrees * .pi / 180
        return SkyPlotPoint(
            x: radius * sin(angle),
            y: -radius * cos(angle)
        )
    }
}
