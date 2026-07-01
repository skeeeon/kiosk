// A small, kiosk-local Leaflet wrapper for the Locations map view
// (docs/location-sightings-plan.md, L4). Deliberately trimmed to what the
// Locations view needs: clustered dots + coarse-area halos over a dark
// basemap. No dynamic/KV-driven markers, no theme switching, no JSONPath — the
// SPA is fixed-dark and the data is a static per-load snapshot.
//
// Markers are vector `L.divIcon` dots (a styled <div>, no image asset) rather
// than Leaflet's default PNG pin, so the map ships no marker images and needs
// no CDN/icon-bundling shim — only the basemap tiles hit the network, which is
// inherent to any web map. Each dot is paired with an `L.circle` halo: the dot
// marks the last-seen point, the halo renders the *coarse area* the sighting
// actually implies (a gateway fix locates the tag to within its read range,
// not to a survey-grade point). Halo radius is real meters, so it grows/shrinks
// with zoom — sub-pixel at fleet zoom (clustering takes over), legible at street
// zoom exactly when someone would otherwise read the dot literally.
import { shallowRef, onUnmounted } from 'vue'
import L from 'leaflet'
import 'leaflet/dist/leaflet.css'
import 'leaflet.markercluster'
import 'leaflet.markercluster/dist/MarkerCluster.css'
import 'leaflet.markercluster/dist/MarkerCluster.Default.css'

// CARTO dark tiles match the SPA's fixed slate palette. Attribution is required
// by both OSM and CARTO's usage terms.
const TILE_URL = 'https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png'
const TILE_ATTRIBUTION =
  '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors &copy; <a href="https://carto.com/attributions">CARTO</a>'

// US centroid — the neutral default before markers are fit. The view calls
// fitAllMarkers() on entry, so this is only ever seen when there are no pins.
const DEFAULT_CENTER: L.LatLngExpression = [39.8283, -98.5795]
const DEFAULT_ZOOM = 4

export interface MapMarkerInput {
  id: string
  lat: number
  lon: number
  label?: string
  // Hex color driven by sighting staleness (fresh → old). Applied to both the
  // dot and its halo so temporal trust reads at a glance.
  color: string
  // Coarse-area radius in meters — the spatial "somewhere around here" the
  // gateway fix implies.
  radiusM: number
}

export function useLeafletMap() {
  const map = shallowRef<L.Map | null>(null)
  const clusterLayer = shallowRef<L.MarkerClusterGroup | null>(null)
  const haloLayer = shallowRef<L.FeatureGroup | null>(null)

  // Idempotent: the view lazily inits on first switch to the map tab, and a
  // re-entry just re-fits. A second init on a live map would leak the old one.
  function initMap(containerId: string) {
    if (map.value) return
    const container = document.getElementById(containerId)
    if (!container) return

    const m = L.map(containerId, {
      center: DEFAULT_CENTER,
      zoom: DEFAULT_ZOOM,
      zoomControl: false,
      attributionControl: true,
    })
    L.control.zoom({ position: 'bottomleft' }).addTo(m)
    L.tileLayer(TILE_URL, { attribution: TILE_ATTRIBUTION, maxZoom: 19 }).addTo(m)

    const cluster = L.markerClusterGroup({
      chunkedLoading: true,
      maxClusterRadius: 50,
      showCoverageOnHover: false,
    })
    cluster.addTo(m)
    // Halos live outside the cluster group (cluster plugin only clusters
    // Markers, not vector layers) so they render independently at every zoom.
    const halos = L.featureGroup().addTo(m)

    map.value = m
    clusterLayer.value = cluster
    haloLayer.value = halos

    // A freshly-shown container can report zero size for a frame; nudge Leaflet
    // once it's laid out so tiles fill the pane.
    setTimeout(() => m.invalidateSize(), 100)
  }

  // A colored dot with a dark ring, styled entirely inline so it needs no CSS
  // (overriding the default `leaflet-div-icon` white box via a fresh class).
  function dotIcon(color: string): L.DivIcon {
    return L.divIcon({
      className: 'kiosk-map-dot',
      html: `<span style="display:block;width:12px;height:12px;border-radius:9999px;background:${color};border:2px solid rgba(15,23,42,0.9);box-shadow:0 0 0 1px ${color}80"></span>`,
      iconSize: [16, 16],
      iconAnchor: [8, 8],
    })
  }

  function renderMarkers(markers: MapMarkerInput[], onClick?: (id: string) => void) {
    const cluster = clusterLayer.value
    const halos = haloLayer.value
    if (!cluster || !halos) return

    cluster.clearLayers()
    halos.clearLayers()

    markers.forEach(({ id, lat, lon, label, color, radiusM }) => {
      L.circle([lat, lon], {
        radius: radiusM,
        color,
        weight: 1,
        opacity: 0.45,
        fillColor: color,
        fillOpacity: 0.12,
        interactive: false, // clicks belong to the dot, not the halo
      }).addTo(halos)

      const marker = L.marker([lat, lon], { icon: dotIcon(color), title: label })
      if (onClick) marker.on('click', () => onClick(id))
      if (label) marker.bindTooltip(label, { direction: 'top', offset: [0, -8] })
      cluster.addLayer(marker)
    })
  }

  // Fit the viewport to all dots. No-op (returns false) when there are none, so
  // the caller keeps the default center rather than fitting empty bounds.
  function fitAllMarkers(): boolean {
    const m = map.value
    const cluster = clusterLayer.value
    if (!m || !cluster) return false
    const bounds = cluster.getBounds()
    if (!bounds.isValid()) return false
    m.fitBounds(bounds, { padding: [40, 40], maxZoom: 16 })
    return true
  }

  function invalidateSize() {
    map.value?.invalidateSize()
  }

  function cleanup() {
    if (map.value) {
      map.value.remove()
      map.value = null
    }
    clusterLayer.value = null
    haloLayer.value = null
  }

  onUnmounted(cleanup)

  return { initMap, renderMarkers, fitAllMarkers, invalidateSize, cleanup }
}
