# AI Video Frame Selector — App Brief

## Goal

Build an app that selects a few still images from a short video based on a natural-language prompt and exports them as downloadable image files.

Target video length: up to 1–2 minutes. Images must come from the original video; image generation is not required.

Example prompt:

> Select 5 sharp images where the product is clearly visible. Prefer different angles and avoid near-duplicate images.

## MVP User Flow

1. Upload a video.
2. Enter a selection prompt and the desired number of images.
3. Run the selection process.
4. Preview the selected images with timestamps.
5. Download individual images or all selected images as a ZIP file.

Start with JPG export. PNG and WebP can be added as optional formats.

## Recommended Model

Start by evaluating **Gemini 3.5 Flash-Lite** (`gemini-3.5-flash-lite`) as the default model. It supports video input and is a low-cost candidate for finding scenes that match a prompt.

Evaluate **Gemini 3.8 Flash** (`gemini-3.8-flash`) as an alternative for more complex prompts if Flash-Lite performs poorly on representative videos. Better results for this particular task must be demonstrated through testing, not assumed.

Keep the model configurable so it can be changed without redesigning the app. Confirm model availability and current pricing before implementation.

## Suggested Processing Pipeline

1. **Validate the upload:** Check file type, file size, duration, and whether the video can be decoded.
2. **Find candidate scenes:** Ask the model to identify time ranges matching the user's prompt. Request structured results containing timestamps and short explanations.
3. **Extract candidate frames:** Sample more densely within those time ranges. A tool such as FFmpeg can extract frames from the original video.
4. **Check image quality:** Filter or rank frames for sharpness and remove near-duplicates. If needed, ask the model to compare a small shortlist for prompt relevance.
5. **Select the final set:** Balance relevance, image quality, and variety across the requested number of images.
6. **Export:** Save the selected original frames and make them available individually and as a ZIP archive.

Treat model timestamps as candidates rather than exact frame references. Validate that they fall within the video and inspect neighboring frames before making the final selection.

Analyze a smaller preview when sufficient, but always export from the original video at its source resolution. Only process audio when the prompt depends on speech or sound.

## Cost Estimates

Reference date: September 23, 2026. Prices are in USD and should be rechecked before launch.

| Model | Input per 1 million tokens | Output per 1 million tokens |
| --- | ---: | ---: |
| Gemini 3.5 Flash-Lite | $0.30 | $2.50 |
| Gemini 3.8 Flash | $0.75 | $3.75 |

The listed Gemini 3.8 Flash rates apply through December 31, 2026. The pricing page lists higher rates starting January 1, 2027. Output pricing includes thinking tokens.

For a rough Flash-Lite estimate, assume 100–300 input tokens per second of video and 1,000 total billable output tokens:

- 120-second video: approximately 12,000–36,000 video input tokens.
- Video input cost: approximately $0.0036–$0.0108.
- Output cost: approximately $0.0025.
- Estimated single analysis: approximately **$0.006–$0.014**, excluding small prompt overhead.

Use **$0.01–$0.05 per 1–2 minute video** as an initial AI planning budget for a limited multi-step workflow. This is an estimate, not a fixed price or upper bound. Higher sampling rates, additional image analysis, reasoning, retries, and repeated requests can increase cost.

| Volume | Estimated AI budget |
| --- | ---: |
| 100 videos | $1–$5 |
| 1,000 videos | $10–$50 |

These estimates exclude development, video processing compute, storage, bandwidth, and taxes. Frame export does not incur an AI image-generation fee, but it still uses processing resources.

Log actual token usage and cost per processing stage to validate the estimates.

## Quality Requirements

- Selected images should match the user's prompt.
- Prefer sharp frames and avoid near-duplicates.
- Preserve the original video's aspect ratio and source resolution on export.
- Fast actions may require denser sampling to capture the best moment.
- If fewer suitable images exist than requested, return fewer images with a clear explanation.
- If no suitable scene is found, report that instead of returning unrelated images.
- The model identifies content; the application performs frame extraction and file creation.

## Initial Validation

Test on 20–30 representative videos before committing to a model or cost target. Include easy prompts, complex prompts, fast motion, low-light footage, and cases with no matching content.

Review prompt relevance, sharpness, duplicate rate, processing time, and actual cost per video. Compare Flash-Lite and Flash on the same examples where quality is uncertain.

## Implementation Notes

- Keep API credentials on the server.
- Put upload limits and processing limits in configuration.
- Show processing progress and clear failure messages.
- Limit retries and additional model calls to control costs.
- Define a retention period and automatically clean up uploaded videos and temporary frames.
- Keep prompt interpretation, frame extraction, quality filtering, and export as separate components.

## Sources

- [Gemini API pricing](https://ai.google.dev/gemini-api/docs/pricing)
- [Gemini video understanding documentation](https://ai.google.dev/gemini-api/docs/video-understanding)

