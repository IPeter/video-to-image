import hashlib

import pytest

from v2i_studio import config, models


def test_config_defaults():
    c = config.load({})
    assert (c.host, c.port, c.threads) == ("127.0.0.1", 4100, 4)
    assert c.max_body_bytes == 40 << 20
    assert c.runner.parallel is True and c.runner.spinning is False
    assert (c.runner.birefnet_threads, c.runner.sam_threads) == config.split_threads(4, True)
    assert c.runner.sam_encoder == "fp32"
    assert c.models_dir.endswith("services/studio/models")
    assert c.manifest.endswith("services/studio/models.sha256")


def test_config_overrides():
    c = config.load(
        {
            "STUDIO_HOST": "0.0.0.0",
            "STUDIO_PORT": "8090",
            "STUDIO_MODELS_DIR": "/models",
            "STUDIO_THREADS": "6",
            "STUDIO_PARALLEL": "false",
            "STUDIO_SAM_THREADS": "5",
            "STUDIO_SAM_ENCODER": "int8",
            "STUDIO_MAX_BODY_BYTES": "1048576",
        }
    )
    assert (c.host, c.port, c.models_dir, c.threads) == ("0.0.0.0", 8090, "/models", 6)
    assert c.runner.parallel is False
    assert (c.runner.birefnet_threads, c.runner.sam_threads) == (6, 5)
    assert c.runner.sam_encoder == "int8" and c.max_body_bytes == 1 << 20


def test_split_threads():
    assert config.split_threads(4, False) == (4, 4)
    assert config.split_threads(1, True) == (1, 1)
    for t in range(2, 9):
        b, s = config.split_threads(t, True)
        assert b + s == t and b >= s


@pytest.mark.parametrize(
    "env",
    [
        {"STUDIO_THREADS": "0"},
        {"STUDIO_THREADS": "four"},
        {"STUDIO_PORT": "70000"},
        {"STUDIO_PARALLEL": "maybe"},
        {"STUDIO_SAM_ENCODER": "fp16"},
    ],
)
def test_config_rejects(env):
    with pytest.raises(ValueError):
        config.load(env)


def _write(tmp_path, name, data):
    p = tmp_path / name
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_bytes(data)
    return hashlib.sha256(data).hexdigest()


def test_verify_models_provenance(tmp_path):
    a = _write(tmp_path, "one.onnx", b"one")
    b = _write(tmp_path, "sam/x.onnx", b"x")
    c = _write(tmp_path, "sam/x.onnx_data", b"xd")
    (tmp_path / "m.sha256").write_text(f"# comment\n{a}  one.onnx\n{b}  sam/x.onnx\n{c} *sam/x.onnx_data\n")
    manifest = models.read_manifest(tmp_path / "m.sha256")
    prov = models.verify_models(tmp_path, {"one": ["one.onnx"], "sam": ["sam/x.onnx", "sam/x.onnx_data"]}, manifest)
    assert prov["one"] == "sha256:" + a
    lines = f"{b}  sam/x.onnx\n{c}  sam/x.onnx_data\n"
    assert prov["sam"] == "sha256:" + hashlib.sha256(lines.encode()).hexdigest()


def test_verify_models_rejects_bad_files(tmp_path):
    a = _write(tmp_path, "one.onnx", b"one")
    with pytest.raises(ValueError, match="sha256"):
        models.verify_models(tmp_path, {"one": ["one.onnx"]}, {"one.onnx": "0" * 64})
    with pytest.raises(ValueError, match="not listed"):
        models.verify_models(tmp_path, {"one": ["one.onnx"]}, {})
    with pytest.raises(FileNotFoundError, match="fetch-models.sh"):
        models.verify_models(tmp_path, {"two": ["two.onnx"]}, {"two.onnx": a})


def test_model_files_follow_the_encoder_choice():
    assert models.model_files("fp32")["sam"][2:] == [
        "sam2.1-hiera-base-plus/vision_encoder.onnx",
        "sam2.1-hiera-base-plus/vision_encoder.onnx_data",
    ]
    assert "sam2.1-hiera-base-plus/vision_encoder_int8.onnx_data" in models.model_files("int8")["sam"]


def test_manifest_in_repo_lists_every_model_file():
    manifest = models.read_manifest(config.SERVICE_DIR / "models.sha256")
    for enc in models.SAM_ENCODERS:
        for names in models.model_files(enc).values():
            assert set(names) <= set(manifest)
