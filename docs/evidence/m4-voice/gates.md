| Gate | Role | Candidate | Lang | Metric | Value | Bar | Runs | Pass |
| --- | --- | --- | --- | --- | ---: | ---: | ---: | --- |
| 1 | stt | whisper-tiny | en | wer | 0.138 | <= 0.15 | 3 | pass |
| 2 | stt | whisper-tiny | en | rtf_median | 0.086 | < 1 | 3 | pass |
| 3 | stt | whisper-tiny | en | worker_peak_rss_mib | 202.109 | <= 500 | 3 | pass |
| 1 | stt | whisper-base | en | wer | 0.104 | <= 0.15 | 3 | pass |
| 2 | stt | whisper-base | en | rtf_median | 0.164 | < 1 | 3 | pass |
| 3 | stt | whisper-base | en | worker_peak_rss_mib | 229.980 | <= 500 | 3 | pass |
| 1 | stt | whisper-small | en | wer | 0.073 | <= 0.15 | 3 | pass |
| 2 | stt | whisper-small | en | rtf_median | 0.547 | < 1 | 3 | pass |
| 3 | stt | whisper-small | en | worker_peak_rss_mib | 550.035 | <= 500 | 3 | FAIL |
| 4 | tts | piper-en-ljspeech | en | round_trip_wer | 0.070 | <= 0.1 | 3 | pass |
| 5 | tts | piper-en-ljspeech | en | ttfa_median_ms | 2292.324 | <= 1500 | 3 | FAIL |
| 6 | tts | piper-en-ljspeech | en | rtf_median | 0.629 | < 1 | 3 | pass |
| 7 | tts | piper-en-ljspeech | en | worker_peak_rss_mib | 644.262 | <= 300 | 3 | FAIL |
| 4 | tts | kokoros-en | en | round_trip_wer | 0.075 | <= 0.1 | 3 | pass |
| 5 | tts | kokoros-en | en | ttfa_median_ms | 1892.204 | <= 1500 | 3 | FAIL |
| 6 | tts | kokoros-en | en | rtf_median | 0.472 | < 1 | 3 | pass |
| 7 | tts | kokoros-en | en | worker_peak_rss_mib | 796.516 | <= 300 | 3 | FAIL |
| 1 | stt | whisper-tiny | it | wer | 0.299 | <= 0.15 | 3 | FAIL |
| 2 | stt | whisper-tiny | it | rtf_median | 0.095 | < 1 | 3 | pass |
| 3 | stt | whisper-tiny | it | worker_peak_rss_mib | 202.109 | <= 500 | 3 | pass |
| 1 | stt | whisper-base | it | wer | 0.197 | <= 0.15 | 3 | FAIL |
| 2 | stt | whisper-base | it | rtf_median | 0.161 | < 1 | 3 | pass |
| 3 | stt | whisper-base | it | worker_peak_rss_mib | 229.980 | <= 500 | 3 | pass |
| 1 | stt | whisper-small | it | wer | 0.083 | <= 0.15 | 3 | pass |
| 2 | stt | whisper-small | it | rtf_median | 0.471 | < 1 | 3 | pass |
| 3 | stt | whisper-small | it | worker_peak_rss_mib | 550.035 | <= 500 | 3 | FAIL |
| 4 | tts | piper-it-riccardo | it | round_trip_wer | 0.164 | <= 0.1 | 3 | FAIL |
| 5 | tts | piper-it-riccardo | it | ttfa_median_ms | 1421.913 | <= 1500 | 3 | pass |
| 6 | tts | piper-it-riccardo | it | rtf_median | 0.433 | < 1 | 3 | pass |
| 7 | tts | piper-it-riccardo | it | worker_peak_rss_mib | 354.605 | <= 300 | 3 | FAIL |
| 4 | tts | piper-it-paola | it | round_trip_wer | 0.153 | <= 0.1 | 3 | FAIL |
| 5 | tts | piper-it-paola | it | ttfa_median_ms | 2029.146 | <= 1500 | 3 | FAIL |
| 6 | tts | piper-it-paola | it | rtf_median | 0.659 | < 1 | 3 | pass |
| 7 | tts | piper-it-paola | it | worker_peak_rss_mib | 576.672 | <= 300 | 3 | FAIL |
| 8 | vad | silero-vad | all | speech_segments_in_nonspeech_clips | 0.000 | == 0 | 3 | pass |
| 9 | vad | silero-vad | all | fleurs_clips_with_speech | 1.000 | >= 0.95 | 3 | pass |
| 10 | vad | silero-vad | all | worker_peak_rss_mib | 41.156 | <= 150 | 3 | pass |
| 11 | coresidency | whisper-tiny+piper-en-ljspeech+silero-vad | all | resident_role_reloads_at_max_active_2 | 8.000 | == 0 | 1 | FAIL (info) |
| 12 | coresidency | whisper-tiny+piper-en-ljspeech+silero-vad | all | r_total_peak_mib_at_max_active_2 | 2394.824 | <= 5811 | 1 | pass (info) |
| 11 | coresidency | whisper-tiny+piper-en-ljspeech+silero-vad | all | resident_role_reloads_at_max_active_5 | 0.000 | == 0 | 1 | pass |
| 12 | coresidency | whisper-tiny+piper-en-ljspeech+silero-vad | all | r_total_peak_mib_at_max_active_5 | 3012.012 | <= 5811 | 1 | pass |
