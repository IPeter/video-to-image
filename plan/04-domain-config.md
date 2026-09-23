# 04 — Domain Configuration

A domain is a YAML file in `configs/domains/`. The backend loads the directory at boot into a
`map[string]DomainConfig`, validates every file, and refuses to start on any error. On k3s the
directory is a **ConfigMap** mounted into both `serve` and `work`. The frontend receives a
**public subset** via `GET /v1/domains/:key` so it never hard-codes limits or labels.

## Go schema

```go
type DomainConfig struct {
    Key            string   `yaml:"key"            validate:"required,alphanum,lowercase"`
    Name           string   `yaml:"name"           validate:"required"`
    AllowedOrigins []string `yaml:"allowedOrigins" validate:"required,min=1,dive,url"`

    Limits struct {
        MaxUploadBytes     int64 `yaml:"maxUploadBytes"     validate:"required"`
        MaxDurationSec     int   `yaml:"maxDurationSec"     validate:"required"`
        MinDurationSec     int   `yaml:"minDurationSec"`
        MinShortSidePx     int   `yaml:"minShortSidePx"`
        MaxCandidateFrames int   `yaml:"maxCandidateFrames" validate:"required"`
        RatePerIPPerHour   int   `yaml:"ratePerIpPerHour"   validate:"required"`
    } `yaml:"limits"`

    Model struct {
        Name           string  `yaml:"name"           validate:"required"`
        Temperature    float32 `yaml:"temperature"`
        ThinkingBudget *int32  `yaml:"thinkingBudget"`
        PreviewHeight  int     `yaml:"previewHeight"  validate:"required"`
        PreviewFPS     int     `yaml:"previewFps"     validate:"required"`
    } `yaml:"model"`

    Gate struct {
        Subject       string  `yaml:"subject"       validate:"required"`
        Instructions  string  `yaml:"instructions"  validate:"required"`
        MinConfidence float64 `yaml:"minConfidence" validate:"gte=0,lte=1"`
    } `yaml:"gate"`

    Shots []Shot `yaml:"shots" validate:"required,min=1,dive"`   // ordered by priority

    Selection struct {
        ImageCount       int     `yaml:"imageCount"       validate:"required,min=1"`
        Instructions     string  `yaml:"instructions"     validate:"required"`
        MaxScenes        int     `yaml:"maxScenes"        validate:"required"`
        SampleFPS        int     `yaml:"sampleFps"        validate:"required"`
        ModelRerank      bool    `yaml:"modelRerank"`
        Weights          struct {
            Relevance float64 `yaml:"relevance"`
            Sharpness float64 `yaml:"sharpness"`
            Exposure  float64 `yaml:"exposure"`
        } `yaml:"weights"`
        DedupeMaxHamming int     `yaml:"dedupeMaxHamming" validate:"gte=0,lte=64"`
    } `yaml:"selection"`

    RetentionHours int `yaml:"retentionHours" validate:"required"`

    Copy struct {
        UploadTitle   string `yaml:"uploadTitle"   validate:"required"`
        UploadHint    string `yaml:"uploadHint"    validate:"required"`
        RejectedTitle string `yaml:"rejectedTitle" validate:"required"`
    } `yaml:"copy"`
}

type Shot struct {
    ID          string `yaml:"id"          validate:"required"`
    Label       string `yaml:"label"       validate:"required"`
    Description string `yaml:"description" validate:"required"`
    Required    bool   `yaml:"required"`
}
```

Extra validation beyond tags: shot IDs unique, weights sum to 1 ± 0.01, `key` equals the file
name, at least one `required` shot.

## First domain: `configs/domains/cars.yaml`

The goal is the set of images a dealership would put on a listing: the standard walkaround
angles plus the interior and detail shots that build buyer trust.

```yaml
key: cars
name: Car Listing Photos
allowedOrigins:
  - http://localhost:3000
  - https://cars.home.example        # k3s ingress host, change to the real domain later

limits:
  maxUploadBytes: 524288000          # 500 MB
  maxDurationSec: 180
  minDurationSec: 5
  minShortSidePx: 480
  maxCandidateFrames: 300
  ratePerIpPerHour: 10

model:
  name: gemini-3.5-flash-lite
  temperature: 0.2
  thinkingBudget: 0
  previewHeight: 480
  previewFps: 2

gate:
  subject: >-
    a single passenger vehicle (car, SUV, van or pickup) being shown or walked around,
    as for a sales listing
  minConfidence: 0.6
  instructions: |
    Pass if the main subject throughout most of the video is one vehicle, filmed from
    outside and/or inside, stationary or slowly moving, in a lot, garage, street or studio.
    Fail if: the video is mainly people, scenery, a house or room, a motorcycle or bicycle,
    a racing or driving clip where the car is not the subject being presented, a screen
    recording, or if multiple different vehicles are shown with no clear single subject.

shots:
  - id: front_three_quarter
    label: Front 3/4
    required: true
    description: Front and one side visible at roughly 45°, whole car in frame; this is the hero shot
  - id: side_profile
    label: Side profile
    required: true
    description: Full side of the car, perpendicular, wheels and roofline fully visible
  - id: rear_three_quarter
    label: Rear 3/4
    required: true
    description: Rear and one side at roughly 45°, whole car in frame
  - id: interior_dashboard
    label: Dashboard
    required: true
    description: Driver's seat view of dashboard, steering wheel and center console
  - id: front
    label: Front
    description: Straight-on front showing grille, headlights and badge
  - id: rear
    label: Rear
    description: Straight-on rear showing tail lights, badge and bumper
  - id: interior_front_seats
    label: Front seats
    description: Front seats and upholstery from an open door
  - id: interior_rear_seats
    label: Rear seats
    description: Rear bench and legroom
  - id: odometer
    label: Odometer
    description: Instrument cluster with mileage readable, screen lit
  - id: wheel
    label: Wheel & tyre
    description: Close-up of one wheel showing rim and tyre condition
  - id: engine_bay
    label: Engine bay
    description: Open hood showing the engine
  - id: trunk
    label: Trunk / boot
    description: Open trunk showing cargo space
  - id: detail
    label: Detail
    description: Badges, headlights, infotainment screen or other notable detail

selection:
  imageCount: 8
  maxScenes: 20
  sampleFps: 4
  modelRerank: false
  dedupeMaxHamming: 6
  weights:
    relevance: 0.5
    sharpness: 0.4
    exposure: 0.1
  instructions: |
    You are selecting moments for a used-car sales listing. Prefer moments where the whole
    car (or the whole interior area) is in frame, the camera is steady, nothing blocks the
    view (no people, hands, or door frames cutting the car), and lighting shows the paint
    and condition honestly. Avoid moments with heavy motion blur, strong glare on windows or
    paint, and moments where the car is cut off by the frame edge. If the video has both
    exterior and interior footage, include both. Only fill shots you can actually see; do
    not guess.

retentionHours: 24

copy:
  uploadTitle: Upload a walkaround video of the car
  uploadHint: 30–120 seconds. Walk slowly around the outside, then film the interior. MP4 or MOV, up to 500 MB.
  rejectedTitle: This doesn't look like a car video
```

Notes on the car domain:
- The three required exterior angles plus the dashboard are the minimum viable listing. If any
  is missing the result still succeeds but the frontend shows "Missing: Rear 3/4" so the user can
  re-shoot.
- `imageCount: 8` leaves room for 4 optional shots after the 4 required ones. Tune after eval.
- Dealership videos often have people walking through frame. The instructions push the model
  away from those moments; sharpness scoring does the rest.
- Localisation later: add a `copy_hu:` block with the same keys and let the frontend request
  `?lang=hu`. Config change only (decision 12).

## Second domain (future): `homes.yaml` — real estate

Sketch only, to prove the config generalizes:
- Gate: "the interior and/or exterior of a single residential property, filmed as a tour".
- Shots: `exterior_front`, `living_room`, `kitchen`, `primary_bedroom`, `bathroom`, `dining`,
  `garden_or_balcony`, `view`, `detail`.
- Selection instructions emphasise wide angles, level horizon, rooms lit and tidy, no people.
- `imageCount: 12`, `previewFps: 1` (slow pans), `retentionHours: 48`.

Nothing in the pipeline changes for this. That is the test of the design.

## Public subset exposed to the frontend

```go
type PublicDomain struct {
    Key        string       `json:"key"`
    Name       string       `json:"name"`
    Limits     PublicLimits `json:"limits"`     // maxUploadBytes, maxDurationSec, minDurationSec
    Shots      []PublicShot `json:"shots"`      // id, label, required
    ImageCount int          `json:"imageCount"`
    Copy       Copy         `json:"copy"`
}
```

Prompts, model names and weights never leave the server.
