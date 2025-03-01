/*
Copyright 2021 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	extage "filippo.io/age"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/hashicorp/vault/api"
	. "github.com/onsi/gomega"
	gt "github.com/onsi/gomega/types"
	"go.mozilla.org/sops/v3"
	sopsage "go.mozilla.org/sops/v3/age"
	"go.mozilla.org/sops/v3/cmd/sops/formats"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/provider"
	"sigs.k8s.io/kustomize/api/resource"
	kustypes "sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta2"
	"github.com/fluxcd/kustomize-controller/internal/sops/age"
	"github.com/fluxcd/pkg/apis/meta"
)

func TestKustomizationReconciler_Decryptor(t *testing.T) {
	g := NewWithT(t)

	cli, err := api.NewClient(api.DefaultConfig())
	g.Expect(err).NotTo(HaveOccurred(), "failed to create vault client")

	// create a master key on the vault transit engine
	path, data := "sops/keys/firstkey", map[string]interface{}{"type": "rsa-4096"}
	_, err = cli.Logical().Write(path, data)
	g.Expect(err).NotTo(HaveOccurred(), "failed to write key")

	// encrypt the testdata vault secret
	cmd := exec.Command("sops", "--hc-vault-transit", cli.Address()+"/v1/sops/keys/firstkey", "--encrypt", "--encrypted-regex", "^(data|stringData)$", "--in-place", "./testdata/sops/secret.vault.yaml")
	err = cmd.Run()
	g.Expect(err).NotTo(HaveOccurred(), "failed to encrypt file")

	// defer the testdata vault secret decryption, to leave a clean testdata vault secret
	defer func() {
		cmd := exec.Command("sops", "--hc-vault-transit", cli.Address()+"/v1/sops/keys/firstkey", "--decrypt", "--encrypted-regex", "^(data|stringData)$", "--in-place", "./testdata/sops/secret.vault.yaml")
		err = cmd.Run()
	}()

	id := "sops-" + randStringRunes(5)

	err = createNamespace(id)
	g.Expect(err).NotTo(HaveOccurred(), "failed to create test namespace")

	err = createKubeConfigSecret(id)
	g.Expect(err).NotTo(HaveOccurred(), "failed to create kubeconfig secret")

	artifactName := "sops-" + randStringRunes(5)
	artifactChecksum, err := createArtifact(testServer, "testdata/sops", artifactName)
	g.Expect(err).ToNot(HaveOccurred())

	overlayArtifactName := "sops-" + randStringRunes(5)
	overlayChecksum, err := createArtifact(testServer, "testdata/test-dotenv", overlayArtifactName)
	g.Expect(err).ToNot(HaveOccurred())

	repositoryName := types.NamespacedName{
		Name:      fmt.Sprintf("sops-%s", randStringRunes(5)),
		Namespace: id,
	}

	overlayRepositoryName := types.NamespacedName{
		Name:      fmt.Sprintf("sops-%s", randStringRunes(5)),
		Namespace: id,
	}

	err = applyGitRepository(repositoryName, artifactName, "main/"+artifactChecksum)
	g.Expect(err).NotTo(HaveOccurred())

	err = applyGitRepository(overlayRepositoryName, overlayArtifactName, "main/"+overlayChecksum)
	g.Expect(err).NotTo(HaveOccurred())

	pgpKey, err := os.ReadFile("testdata/sops/pgp.asc")
	g.Expect(err).ToNot(HaveOccurred())
	ageKey, err := os.ReadFile("testdata/sops/age.txt")
	g.Expect(err).ToNot(HaveOccurred())

	sopsSecretKey := types.NamespacedName{
		Name:      "sops-" + randStringRunes(5),
		Namespace: id,
	}

	sopsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sopsSecretKey.Name,
			Namespace: sopsSecretKey.Namespace,
		},
		StringData: map[string]string{
			"pgp.asc":          string(pgpKey),
			"age.agekey":       string(ageKey),
			"sops.vault-token": "secret",
		},
	}

	g.Expect(k8sClient.Create(context.Background(), sopsSecret)).To(Succeed())

	kustomizationKey := types.NamespacedName{
		Name:      fmt.Sprintf("sops-%s", randStringRunes(5)),
		Namespace: id,
	}
	kustomization := &kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kustomizationKey.Name,
			Namespace: kustomizationKey.Namespace,
		},
		Spec: kustomizev1.KustomizationSpec{
			Interval: metav1.Duration{Duration: 2 * time.Minute},
			Path:     "./",
			KubeConfig: &kustomizev1.KubeConfig{
				SecretRef: meta.LocalObjectReference{
					Name: "kubeconfig",
				},
			},
			SourceRef: kustomizev1.CrossNamespaceSourceReference{
				Name:      repositoryName.Name,
				Namespace: repositoryName.Namespace,
				Kind:      sourcev1.GitRepositoryKind,
			},
			Decryption: &kustomizev1.Decryption{
				Provider: "sops",
				SecretRef: &meta.LocalObjectReference{
					Name: sopsSecretKey.Name,
				},
			},
			TargetNamespace: id,
		},
	}
	g.Expect(k8sClient.Create(context.TODO(), kustomization)).To(Succeed())

	g.Eventually(func() bool {
		var obj kustomizev1.Kustomization
		_ = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(kustomization), &obj)
		return obj.Status.LastAppliedRevision == "main/"+artifactChecksum
	}, timeout, time.Second).Should(BeTrue())

	overlayKustomizationName := fmt.Sprintf("sops-%s", randStringRunes(5))
	overlayKs := kustomization.DeepCopy()
	overlayKs.ResourceVersion = ""
	overlayKs.Name = overlayKustomizationName
	overlayKs.Spec.SourceRef.Name = overlayRepositoryName.Name
	overlayKs.Spec.SourceRef.Namespace = overlayRepositoryName.Namespace
	overlayKs.Spec.Path = "./testdata/test-dotenv/overlays"

	g.Expect(k8sClient.Create(context.TODO(), overlayKs)).To(Succeed())

	g.Eventually(func() bool {
		var obj kustomizev1.Kustomization
		_ = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(overlayKs), &obj)
		return obj.Status.LastAppliedRevision == "main/"+overlayChecksum
	}, timeout, time.Second).Should(BeTrue())

	t.Run("decrypts SOPS secrets", func(t *testing.T) {
		g := NewWithT(t)

		var pgpSecret corev1.Secret
		g.Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "sops-pgp", Namespace: id}, &pgpSecret)).To(Succeed())
		g.Expect(pgpSecret.Data["secret"]).To(Equal([]byte(`my-sops-pgp-secret`)))

		var ageSecret corev1.Secret
		g.Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "sops-age", Namespace: id}, &ageSecret)).To(Succeed())
		g.Expect(ageSecret.Data["secret"]).To(Equal([]byte(`my-sops-age-secret`)))

		var daySecret corev1.Secret
		g.Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "sops-day", Namespace: id}, &daySecret)).To(Succeed())
		g.Expect(string(daySecret.Data["secret"])).To(Equal("day=Tuesday\n"))

		var yearSecret corev1.Secret
		g.Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "sops-year", Namespace: id}, &yearSecret)).To(Succeed())
		g.Expect(string(yearSecret.Data["year"])).To(Equal("2017"))

		var unencryptedSecret corev1.Secret
		g.Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "unencrypted-sops-year", Namespace: id}, &unencryptedSecret)).To(Succeed())
		g.Expect(string(unencryptedSecret.Data["year"])).To(Equal("2021"))

		var year1Secret corev1.Secret
		g.Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "sops-year1", Namespace: id}, &year1Secret)).To(Succeed())
		g.Expect(string(year1Secret.Data["year"])).To(Equal("year1"))

		var year2Secret corev1.Secret
		g.Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "sops-year2", Namespace: id}, &year2Secret)).To(Succeed())
		g.Expect(string(year2Secret.Data["year"])).To(Equal("year2"))

		var encodedSecret corev1.Secret
		g.Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "sops-month", Namespace: id}, &encodedSecret)).To(Succeed())
		g.Expect(string(encodedSecret.Data["month.yaml"])).To(Equal("month: May\n"))

		var hcvaultSecret corev1.Secret
		g.Expect(k8sClient.Get(context.TODO(), types.NamespacedName{Name: "sops-hcvault", Namespace: id}, &hcvaultSecret)).To(Succeed())
		g.Expect(string(hcvaultSecret.Data["secret"])).To(Equal("my-sops-vault-secret\n"))
	})

	t.Run("does not emit change events for identical secrets", func(t *testing.T) {
		g := NewWithT(t)

		resultK := &kustomizev1.Kustomization{}
		revision := "v2.0.0"
		err = applyGitRepository(repositoryName, artifactName, revision)
		g.Expect(err).NotTo(HaveOccurred())

		g.Eventually(func() bool {
			_ = k8sClient.Get(context.Background(), client.ObjectKeyFromObject(kustomization), resultK)
			return resultK.Status.LastAttemptedRevision == revision
		}, timeout, time.Second).Should(BeTrue())

		events := getEvents(resultK.GetName(), map[string]string{"kustomize.toolkit.fluxcd.io/revision": revision})
		g.Expect(len(events)).To(BeIdenticalTo(1))
		g.Expect(events[0].Message).Should(ContainSubstring("Reconciliation finished"))
		g.Expect(events[0].Message).ShouldNot(ContainSubstring("configured"))
	})
}

func TestIsEncryptedSecret(t *testing.T) {
	tests := []struct {
		name   string
		object []byte
		want   gt.GomegaMatcher
	}{
		{name: "encrypted secret", object: []byte("apiVersion: v1\nkind: Secret\nsops: true\n"), want: BeTrue()},
		{name: "decrypted secret", object: []byte("apiVersion: v1\nkind: Secret\n"), want: BeFalse()},
		{name: "other resource", object: []byte("apiVersion: v1\nkind: Deployment\n"), want: BeFalse()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			u := &unstructured.Unstructured{}
			g.Expect(yaml.Unmarshal(tt.object, u)).To(Succeed())
			g.Expect(IsEncryptedSecret(u)).To(tt.want)
		})
	}
}

func TestKustomizeDecryptor_ImportKeys(t *testing.T) {
	g := NewWithT(t)

	const provider = "sops"

	pgpKey, err := os.ReadFile("testdata/sops/pgp.asc")
	g.Expect(err).ToNot(HaveOccurred())
	ageKey, err := os.ReadFile("testdata/sops/age.txt")
	g.Expect(err).ToNot(HaveOccurred())

	tests := []struct {
		name        string
		decryption  *kustomizev1.Decryption
		secret      *corev1.Secret
		wantErr     bool
		inspectFunc func(g *GomegaWithT, decryptor *KustomizeDecryptor)
	}{
		{
			name: "PGP key",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "pgp-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pgp-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"pgp" + DecryptionPGPExt: pgpKey,
				},
			},
		},
		{
			name: "PGP key import error",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "pgp-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pgp-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"pgp" + DecryptionPGPExt: []byte("not-a-valid-armored-key"),
				},
			},
			wantErr: true,
		},
		{
			name: "age key",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "age-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "age-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"age" + DecryptionAgeExt: ageKey,
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *KustomizeDecryptor) {
				g.Expect(decryptor.ageIdentities).To(HaveLen(1))
			},
		},
		{
			name: "age key import error",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "age-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "age-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"age" + DecryptionAgeExt: []byte("not-a-valid-key"),
				},
			},
			wantErr: true,
			inspectFunc: func(g *GomegaWithT, decryptor *KustomizeDecryptor) {
				g.Expect(decryptor.ageIdentities).To(HaveLen(0))
			},
		},
		{
			name: "HC Vault token",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "hcvault-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hcvault-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionVaultTokenFileName: []byte("some-hcvault-token"),
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *KustomizeDecryptor) {
				g.Expect(decryptor.vaultToken).To(Equal("some-hcvault-token"))
			},
		},
		{
			name: "Azure Key Vault token",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "azkv-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "azkv-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionAzureAuthFile: []byte(`tenantId: some-tenant-id
clientId: some-client-id
clientSecret: some-client-secret`),
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *KustomizeDecryptor) {
				g.Expect(decryptor.azureToken).ToNot(BeNil())
			},
		},
		{
			name: "Azure Key Vault token load config error",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "azkv-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "azkv-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionAzureAuthFile: []byte(`{"malformed\: JSON"}`),
				},
			},
			wantErr: true,
			inspectFunc: func(g *GomegaWithT, decryptor *KustomizeDecryptor) {
				g.Expect(decryptor.azureToken).To(BeNil())
			},
		},
		{
			name: "Azure Key Vault unsupported config",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "azkv-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "azkv-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					DecryptionAzureAuthFile: []byte(`tenantId: incomplete`),
				},
			},
			wantErr: true,
			inspectFunc: func(g *GomegaWithT, decryptor *KustomizeDecryptor) {
				g.Expect(decryptor.azureToken).To(BeNil())
			},
		},
		{
			name: "multiple Secret data entries",
			decryption: &kustomizev1.Decryption{
				Provider: provider,
				SecretRef: &meta.LocalObjectReference{
					Name: "multiple-secret",
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "multiple-secret",
					Namespace: provider,
				},
				Data: map[string][]byte{
					"age" + DecryptionAgeExt:     ageKey,
					DecryptionVaultTokenFileName: []byte("some-hcvault-token"),
				},
			},
			inspectFunc: func(g *GomegaWithT, decryptor *KustomizeDecryptor) {
				g.Expect(decryptor.vaultToken).ToNot(BeEmpty())
				g.Expect(decryptor.ageIdentities).To(HaveLen(1))
			},
		},
		{
			name:       "no Decryption spec",
			decryption: nil,
			wantErr:    false,
		},
		{
			name: "no Decryption Secret",
			decryption: &kustomizev1.Decryption{
				Provider: DecryptionProviderSOPS,
			},
			wantErr: false,
		},
		{
			name: "non-existing Decryption Secret",
			decryption: &kustomizev1.Decryption{
				Provider: DecryptionProviderSOPS,
				SecretRef: &meta.LocalObjectReference{
					Name: "does-not-exist",
				},
			},
			wantErr: true,
		},
		{
			name: "unimplemented Decryption Provider",
			decryption: &kustomizev1.Decryption{
				Provider: "not-supported",
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			cb := fake.NewClientBuilder()
			if tt.secret != nil {
				cb.WithObjects(tt.secret)
			}
			kustomization := kustomizev1.Kustomization{
				ObjectMeta: metav1.ObjectMeta{
					Name:      provider + "-" + tt.name,
					Namespace: provider,
				},
				Spec: kustomizev1.KustomizationSpec{
					Interval:   metav1.Duration{Duration: 2 * time.Minute},
					Path:       "./",
					Decryption: tt.decryption,
				},
			}

			d, cleanup, err := NewTempDecryptor("", cb.Build(), kustomization)
			g.Expect(err).ToNot(HaveOccurred())
			t.Cleanup(cleanup)

			match := Succeed()
			if tt.wantErr {
				match = HaveOccurred()
			}
			g.Expect(d.ImportKeys(context.TODO())).To(match)

			if tt.inspectFunc != nil {
				tt.inspectFunc(g, d)
			}
		})
	}
}

func TestKustomizeDecryptor_SopsDecryptWithFormat(t *testing.T) {
	t.Run("decrypt INI to INI", func(t *testing.T) {
		g := NewWithT(t)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())

		kd := &KustomizeDecryptor{
			checkSopsMac:  true,
			ageIdentities: age.ParsedIdentities{ageID},
		}

		format := formats.Ini
		data := []byte("[config]\nkey = value\n\n")
		encData, err := kd.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&sopsage.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, data, format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(bytes.Contains(encData, sopsFormatToMarkerBytes[format])).To(BeTrue())
		g.Expect(encData).ToNot(Equal(data))

		out, err := kd.SopsDecryptWithFormat(encData, format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(out).To(Equal(data))
	})

	t.Run("decrypt JSON to YAML", func(t *testing.T) {
		g := NewWithT(t)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())

		kd := &KustomizeDecryptor{
			checkSopsMac:  true,
			ageIdentities: age.ParsedIdentities{ageID},
		}

		inputFormat, outputFormat := formats.Json, formats.Yaml
		data := []byte("{\"key\": \"value\"}\n")
		encData, err := kd.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&sopsage.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, data, inputFormat, inputFormat)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(bytes.Contains(encData, sopsFormatToMarkerBytes[inputFormat])).To(BeTrue())

		out, err := kd.SopsDecryptWithFormat(encData, inputFormat, outputFormat)
		t.Logf("%s", out)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(out).To(Equal([]byte("key: value\n")))
	})

	t.Run("invalid JSON data", func(t *testing.T) {
		g := NewWithT(t)

		format := formats.Json
		data, err := (&KustomizeDecryptor{}).SopsDecryptWithFormat([]byte("invalid json"), format, format)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to load encrypted JSON data"))
		g.Expect(data).To(BeNil())
	})

	t.Run("no data key", func(t *testing.T) {
		g := NewWithT(t)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())

		kd := &KustomizeDecryptor{}

		format := formats.Binary
		encData, err := kd.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&sopsage.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, []byte("foo bar"), format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(bytes.Contains(encData, sopsFormatToMarkerBytes[format])).To(BeTrue())

		data, err := kd.SopsDecryptWithFormat(encData, format, format)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("cannot get sops data key"))
		g.Expect(data).To(BeNil())
	})

	t.Run("with mac check", func(t *testing.T) {
		g := NewWithT(t)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())

		kd := &KustomizeDecryptor{
			checkSopsMac:  true,
			ageIdentities: age.ParsedIdentities{ageID},
		}

		format := formats.Dotenv
		data := []byte("key=value\n")
		encData, err := kd.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&sopsage.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, data, format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(bytes.Contains(encData, sopsFormatToMarkerBytes[format])).To(BeTrue())

		out, err := kd.SopsDecryptWithFormat(encData, format, format)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(out).To(Equal(data))

		badMAC := regexp.MustCompile("(?m)[\r\n]+^.*sops_mac=.*$")
		badMACData := badMAC.ReplaceAll(encData, []byte("\nsops_mac=\n"))
		out, err = kd.SopsDecryptWithFormat(badMACData, format, format)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to verify sops data integrity: expected mac 'no MAC'"))
		g.Expect(out).To(BeNil())
	})
}

func TestKustomizeDecryptor_DecryptResource(t *testing.T) {
	var (
		resourceFactory = provider.NewDefaultDepProvider().GetResourceFactory()
		emptyResource   = resourceFactory.FromMap(map[string]interface{}{})
	)

	newSecretResource := func(namespace, name string, data map[string]interface{}) *resource.Resource {
		return resourceFactory.FromMap(map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]interface{}{
				"name":      "secret",
				"namespace": "test",
			},
			"data": data,
		})
	}

	kustomization := kustomizev1.Kustomization{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "decrypt",
			Namespace: "decrypt",
		},
		Spec: kustomizev1.KustomizationSpec{
			Interval: metav1.Duration{Duration: 2 * time.Minute},
			Path:     "./",
		},
	}

	t.Run("SOPS encrypted resource", func(t *testing.T) {
		g := NewWithT(t)

		kus := kustomization.DeepCopy()
		kus.Spec.Decryption = &kustomizev1.Decryption{
			Provider: DecryptionProviderSOPS,
		}

		d, cleanup, err := NewTempDecryptor("", fake.NewClientBuilder().Build(), *kus)
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())
		d.ageIdentities = append(d.ageIdentities, ageID)

		secret := newSecretResource("test", "secret", map[string]interface{}{
			"key": "value",
		})
		g.Expect(isSOPSEncryptedResource(secret)).To(BeFalse())

		secretData, err := secret.MarshalJSON()
		g.Expect(err).ToNot(HaveOccurred())

		encData, err := d.sopsEncryptWithFormat(sops.Metadata{
			EncryptedRegex: "^(data|stringData)$",
			KeyGroups: []sops.KeyGroup{
				{&sopsage.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, secretData, formats.Json, formats.Json)
		g.Expect(err).ToNot(HaveOccurred())

		g.Expect(secret.UnmarshalJSON(encData)).To(Succeed())
		g.Expect(isSOPSEncryptedResource(secret)).To(BeTrue())

		got, err := d.DecryptResource(secret)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).ToNot(BeNil())
		g.Expect(got.MarshalJSON()).To(Equal(secretData))
	})

	t.Run("SOPS encrypted binary Secret data field", func(t *testing.T) {
		g := NewWithT(t)

		kus := kustomization.DeepCopy()
		kus.Spec.Decryption = &kustomizev1.Decryption{
			Provider: DecryptionProviderSOPS,
		}

		d, cleanup, err := NewTempDecryptor("", fake.NewClientBuilder().Build(), *kus)
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())
		d.ageIdentities = append(d.ageIdentities, ageID)

		plainData := []byte("[config]\napp = secret\n\n")
		encData, err := d.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&sopsage.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, plainData, formats.Ini, formats.Yaml)
		g.Expect(err).ToNot(HaveOccurred())

		secret := newSecretResource("test", "secret-data", map[string]interface{}{
			"file.ini": base64.StdEncoding.EncodeToString(encData),
		})
		g.Expect(isSOPSEncryptedResource(secret)).To(BeFalse())

		got, err := d.DecryptResource(secret)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).ToNot(BeNil())
		g.Expect(got.GetDataMap()).To(HaveKeyWithValue("file.ini", base64.StdEncoding.EncodeToString(plainData)))
	})

	t.Run("SOPS encrypted YAML Secret data field", func(t *testing.T) {
		g := NewWithT(t)

		kus := kustomization.DeepCopy()
		kus.Spec.Decryption = &kustomizev1.Decryption{
			Provider: DecryptionProviderSOPS,
		}

		d, cleanup, err := NewTempDecryptor("", fake.NewClientBuilder().Build(), *kus)
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		ageID, err := extage.GenerateX25519Identity()
		g.Expect(err).ToNot(HaveOccurred())
		d.ageIdentities = append(d.ageIdentities, ageID)

		plainData := []byte("structured:\n    data:\n        key: value\n")
		encData, err := d.sopsEncryptWithFormat(sops.Metadata{
			KeyGroups: []sops.KeyGroup{
				{&sopsage.MasterKey{Recipient: ageID.Recipient().String()}},
			},
		}, plainData, formats.Yaml, formats.Yaml)
		g.Expect(err).ToNot(HaveOccurred())

		secret := newSecretResource("test", "secret-data", map[string]interface{}{
			"key.yaml": base64.StdEncoding.EncodeToString(encData),
		})
		g.Expect(isSOPSEncryptedResource(secret)).To(BeFalse())

		got, err := d.DecryptResource(secret)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).ToNot(BeNil())
		g.Expect(got.GetDataMap()).To(HaveKeyWithValue("key.yaml", base64.StdEncoding.EncodeToString(plainData)))
	})

	t.Run("nil resource", func(t *testing.T) {
		g := NewWithT(t)

		d, cleanup, err := NewTempDecryptor("", fake.NewClientBuilder().Build(), *kustomization.DeepCopy())
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		got, err := d.DecryptResource(nil)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(BeNil())
	})

	t.Run("no decryption spec", func(t *testing.T) {
		g := NewWithT(t)

		d, cleanup, err := NewTempDecryptor("", fake.NewClientBuilder().Build(), *kustomization.DeepCopy())
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		got, err := d.DecryptResource(emptyResource.DeepCopy())
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(BeNil())
	})

	t.Run("unimplemented decryption provider", func(t *testing.T) {
		g := NewWithT(t)

		kus := kustomization.DeepCopy()
		kus.Spec.Decryption = &kustomizev1.Decryption{
			Provider: "not-supported",
		}
		d, cleanup, err := NewTempDecryptor("", fake.NewClientBuilder().Build(), *kus)
		g.Expect(err).ToNot(HaveOccurred())
		t.Cleanup(cleanup)

		got, err := d.DecryptResource(emptyResource.DeepCopy())
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(got).To(BeNil())
	})
}

func TestKustomizeDecryptor_decryptKustomizationEnvSources(t *testing.T) {
	type file struct {
		name       string
		symlink    string
		data       []byte
		encrypt    bool
		expectData bool
	}
	tests := []struct {
		name            string
		wordirSuffix    string
		path            string
		files           []file
		secretGenerator []kustypes.SecretArgs
		expectVisited   []string
		wantErr         error
	}{
		{
			name: "decrypt env sources",
			path: "subdir",
			files: []file{
				{name: "subdir/app.env", data: []byte("var1=value1\n"), encrypt: true, expectData: true},
				{name: "subdir/file.txt", data: []byte("file"), encrypt: true, expectData: true},
				{name: "secret.env", data: []byte("var2=value2\n"), encrypt: true, expectData: true},
			},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							FileSources: []string{"file.txt"},
							EnvSources:  []string{"app.env", "key=../secret.env"},
						},
					},
				},
			},
			expectVisited: []string{"subdir/app.env", "subdir/file.txt", "secret.env"},
		},
		{
			name:  "decryption error",
			files: []file{},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							EnvSources: []string{"file.txt"},
						},
					},
				},
			},
			expectVisited: []string{},
			wantErr:       &fs.PathError{Op: "lstat", Path: "file.txt", Err: fmt.Errorf("")},
		},
		{
			name: "follows relative symlink within root",
			path: "subdir",
			files: []file{
				{name: "subdir/symlink", symlink: "../otherdir/data.env"},
				{name: "otherdir/data.env", data: []byte("key=value\n"), encrypt: true, expectData: true},
			},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							EnvSources: []string{"symlink"},
						},
					},
				},
			},
			expectVisited: []string{"otherdir/data.env"},
		},
		{
			name:         "error on symlink outside root",
			wordirSuffix: "subdir",
			path:         "./",
			files: []file{
				{name: "subdir/symlink", symlink: "../otherdir/data.env"},
				{name: "otherdir/data.env", data: []byte("key=value\n"), encrypt: true, expectData: false},
			},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							EnvSources: []string{"symlink"},
						},
					},
				},
			},
			wantErr:       &fs.PathError{Op: "lstat", Path: "otherdir/data.env", Err: fmt.Errorf("")},
			expectVisited: []string{},
		},
		{
			name:         "error on reference outside root",
			wordirSuffix: "subdir",
			path:         "./",
			files: []file{
				{name: "data.env", data: []byte("key=value\n"), encrypt: true, expectData: false},
			},
			secretGenerator: []kustypes.SecretArgs{
				{
					GeneratorArgs: kustypes.GeneratorArgs{
						Name: "envSecret",
						KvPairSources: kustypes.KvPairSources{
							EnvSources: []string{"../data.env"},
						},
					},
				},
			},
			wantErr:       &fs.PathError{Op: "lstat", Path: "data.env", Err: fmt.Errorf("")},
			expectVisited: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tmpDir := t.TempDir()
			root := filepath.Join(tmpDir, tt.wordirSuffix)

			id, err := extage.GenerateX25519Identity()
			g.Expect(err).ToNot(HaveOccurred())
			ageIdentities := age.ParsedIdentities{id}

			d := &KustomizeDecryptor{
				root:          root,
				ageIdentities: ageIdentities,
			}

			for _, f := range tt.files {
				fPath := filepath.Join(tmpDir, f.name)
				g.Expect(os.MkdirAll(filepath.Dir(fPath), 0o700)).To(Succeed())
				if f.symlink != "" {
					g.Expect(os.Symlink(f.symlink, fPath)).To(Succeed())
					continue
				}
				data := f.data
				if f.encrypt {
					format := formats.FormatForPath(f.name)
					data, err = d.sopsEncryptWithFormat(sops.Metadata{
						KeyGroups: []sops.KeyGroup{
							{&sopsage.MasterKey{Recipient: id.Recipient().String()}},
						},
					}, f.data, format, format)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(data).ToNot(Equal(f.data))
				}
				g.Expect(os.WriteFile(fPath, data, 0o644)).To(Succeed())
			}

			visited := make(map[string]struct{}, 0)
			visit := d.decryptKustomizationEnvSources(visited)
			kus := &kustypes.Kustomization{SecretGenerator: tt.secretGenerator}

			err = visit(root, tt.path, kus)
			if tt.wantErr == nil {
				g.Expect(err).ToNot(HaveOccurred())
			} else {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err).To(BeAssignableToTypeOf(tt.wantErr))
				g.Expect(err.Error()).To(ContainSubstring(tt.wantErr.Error()))
			}

			for _, f := range tt.files {
				if f.symlink != "" {
					continue
				}

				b, err := os.ReadFile(filepath.Join(tmpDir, f.name))
				g.Expect(err).ToNot(HaveOccurred())
				if f.expectData {
					g.Expect(b).To(Equal(f.data))
				} else {
					g.Expect(b).ToNot(Equal(f.data))
				}
			}

			absVisited := make(map[string]struct{}, 0)
			for _, v := range tt.expectVisited {
				absVisited[filepath.Join(tmpDir, v)] = struct{}{}
			}
			g.Expect(visited).To(Equal(absVisited))
		})
	}
}

func TestKustomizeDecryptor_decryptSopsFile(t *testing.T) {
	g := NewWithT(t)

	id, err := extage.GenerateX25519Identity()
	g.Expect(err).ToNot(HaveOccurred())
	ageIdentities := age.ParsedIdentities{id}

	type file struct {
		name       string
		symlink    string
		data       []byte
		encrypt    bool
		format     formats.Format
		expectData bool
	}
	tests := []struct {
		name          string
		ageIdentities age.ParsedIdentities
		maxFileSize   int64
		files         []file
		path          string
		format        formats.Format
		wantErr       error
	}{
		{
			name:          "decrypt dotenv file",
			ageIdentities: age.ParsedIdentities{id},
			files: []file{
				{name: "app.env", data: []byte("app=key\n"), encrypt: true, format: formats.Dotenv, expectData: true},
			},
			path:   "app.env",
			format: formats.Dotenv,
		},
		{
			name:          "decrypt YAML file",
			ageIdentities: age.ParsedIdentities{id},
			files: []file{
				{name: "app.yaml", data: []byte("app: key\n"), encrypt: true, format: formats.Yaml, expectData: true},
			},
			path:   "app.yaml",
			format: formats.Yaml,
		},
		{
			name:    "irregular file",
			files:   []file{},
			wantErr: fmt.Errorf("cannot decrypt irregular file as it has file mode type bits set"),
		},
		{
			name:        "file exceeds max size",
			maxFileSize: 5,
			files: []file{
				{name: "app.env", data: []byte("app=key\n"), encrypt: true, format: formats.Dotenv, expectData: false},
			},
			path:    "app.env",
			wantErr: fmt.Errorf("cannot decrypt file with size (972 bytes) exceeding limit (5)"),
		},
		{
			name: "wrong file format",
			files: []file{
				{name: "app.ini", data: []byte("[app]\nkey = value"), encrypt: true, format: formats.Ini, expectData: false},
			},
			path: "app.ini",
		},
		{
			name: "does not follow symlink",
			files: []file{
				{name: "link", symlink: "../"},
			},
			path:    "link",
			wantErr: fmt.Errorf("cannot decrypt irregular file as it has file mode type bits set"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tmpDir := t.TempDir()

			d := &KustomizeDecryptor{
				root:          tmpDir,
				maxFileSize:   maxEncryptedFileSize,
				ageIdentities: ageIdentities,
			}
			if tt.maxFileSize != 0 {
				d.maxFileSize = tt.maxFileSize
			}

			for _, f := range tt.files {
				fPath := filepath.Join(tmpDir, f.name)
				if f.symlink != "" {
					g.Expect(os.Symlink(f.symlink, fPath)).To(Succeed())
					continue
				}
				data := f.data
				if f.encrypt {
					b, err := d.sopsEncryptWithFormat(sops.Metadata{
						KeyGroups: []sops.KeyGroup{
							{&sopsage.MasterKey{Recipient: id.Recipient().String()}},
						},
					}, data, f.format, f.format)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(b).ToNot(Equal(f.data))
					data = b
				}
				g.Expect(os.MkdirAll(filepath.Dir(fPath), 0o700)).To(Succeed())
				g.Expect(os.WriteFile(fPath, data, 0o644)).To(Succeed())
			}

			path := filepath.Join(tmpDir, tt.path)
			err := d.sopsDecryptFile(path, tt.format, tt.format)
			if tt.wantErr != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err).To(BeAssignableToTypeOf(tt.wantErr))
				g.Expect(err.Error()).To(ContainSubstring(tt.wantErr.Error()))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}
			for _, f := range tt.files {
				if f.symlink != "" {
					continue
				}

				b, err := os.ReadFile(filepath.Join(tmpDir, f.name))
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(bytes.Compare(f.data, b) == 0).To(Equal(f.expectData))
			}
		})
	}
}

func Test_secureLoadKustomizationFile(t *testing.T) {
	kusType := kustypes.TypeMeta{
		APIVersion: kustypes.KustomizationVersion,
		Kind:       kustypes.KustomizationKind,
	}
	type file struct {
		name    string
		symlink string
		data    []byte
	}
	tests := []struct {
		name       string
		rootSuffix string
		files      []file
		path       string
		want       *kustypes.Kustomization
		wantErr    error
	}{
		{
			name: "loads default kustomization file",
			files: []file{
				{name: konfig.DefaultKustomizationFileName(), data: []byte("resources:\n- resource.yaml")},
			},
			path: "./",
			want: &kustypes.Kustomization{
				TypeMeta:  kusType,
				Resources: []string{"resource.yaml"},
			},
		},
		{
			name: "loads recognized kustomization file",
			files: []file{
				{name: konfig.RecognizedKustomizationFileNames()[1], data: []byte("resources:\n- resource.yaml")},
			},
			path: "./",
			want: &kustypes.Kustomization{
				TypeMeta:  kusType,
				Resources: []string{"resource.yaml"},
			},
		},
		{
			name: "error on ambitious file match",
			files: []file{
				{name: konfig.RecognizedKustomizationFileNames()[0], data: []byte("resources:\n- resource.yaml")},
				{name: konfig.RecognizedKustomizationFileNames()[1], data: []byte("resources:\n- resource.yaml")},
			},
			path:    "./",
			wantErr: fmt.Errorf("found multiple kustomization files"),
		},
		{
			name:    "error on no file found",
			files:   []file{},
			path:    "./",
			wantErr: fmt.Errorf("no kustomization file found"),
		},
		{
			name:       "error on symlink outside root",
			rootSuffix: "subdir",
			files: []file{
				{name: konfig.DefaultKustomizationFileName(), data: []byte("resources:\n- resource.yaml")},
				{name: "subdir/" + konfig.DefaultKustomizationFileName(), symlink: "../kustomization.yaml"},
			},
			wantErr: fmt.Errorf("no kustomization file found"),
		},
		{
			name: "error on invalid file",
			files: []file{
				{name: konfig.DefaultKustomizationFileName(), data: []byte("resources")},
			},
			wantErr: fmt.Errorf("failed to unmarshal kustomization file"),
		},
		{
			name:    "error on absolute path",
			path:    "/absolute/",
			wantErr: fmt.Errorf("path '/absolute/' must be relative"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tmpDir := t.TempDir()
			for _, f := range tt.files {
				fPath := filepath.Join(tmpDir, f.name)
				if f.symlink != "" {
					g.Expect(os.Symlink(f.symlink, fPath))
					continue
				}
				g.Expect(os.MkdirAll(filepath.Dir(fPath), 0o700)).To(Succeed())
				g.Expect(os.WriteFile(fPath, f.data, 0o644)).To(Succeed())
			}

			root := filepath.Join(tmpDir, tt.rootSuffix)
			got, err := secureLoadKustomizationFile(root, tt.path)
			if wantErr := tt.wantErr; wantErr != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err.Error()).To(ContainSubstring(wantErr.Error()))
				g.Expect(got).To(BeNil())
				return
			}

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func Test_recurseKustomizationFiles(t *testing.T) {
	type kusNode struct {
		path          string
		symlink       string
		resources     []string
		visitErr      error
		visited       int
		expectVisited int
		expectCached  bool
	}
	tests := []struct {
		name         string
		wordirSuffix string
		path         string
		nodes        []*kusNode
		wantErr      error
		wantErrStr   string
	}{
		{
			name:         "recurse on resources",
			wordirSuffix: "foo",
			path:         "bar",
			nodes: []*kusNode{
				{
					path:          "foo/bar/kustomization.yaml",
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "foo/baz/kustomization.yaml",
					resources:     []string{"<tmpdir>/foo/bar/baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "foo/bar/baz/kustomization.yaml",
					resources:     []string{},
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name:         "recursive loop",
			wordirSuffix: "foo",
			path:         "bar",
			nodes: []*kusNode{
				{
					path:          "foo/bar/kustomization.yaml",
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "foo/baz/kustomization.yaml",
					resources:     []string{"../foobar"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "foo/foobar/kustomization.yaml",
					resources:     []string{"../bar"},
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name: "absolute symlink",
			path: "bar",
			nodes: []*kusNode{
				{
					path:          "bar/baz/kustomization.yaml",
					resources:     []string{"../bar/absolute"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:    "bar/absolute",
					symlink: "<tmpdir>/bar/foo/",
				},
				{
					path:          "bar/foo/kustomization.yaml",
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name: "relative symlink",
			path: "bar",
			nodes: []*kusNode{
				{
					path:          "bar/baz/kustomization.yaml",
					resources:     []string{"../bar/relative"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:    "bar/relative",
					symlink: "../foo/",
				},
				{
					path:          "bar/foo/kustomization.yaml",
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name: "recognized kustomization names",
			path: "./",
			nodes: []*kusNode{
				{
					path:          konfig.RecognizedKustomizationFileNames()[1],
					resources:     []string{"bar"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          filepath.Join("bar", konfig.RecognizedKustomizationFileNames()[0]),
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          filepath.Join("baz", konfig.RecognizedKustomizationFileNames()[2]),
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name:       "path does not exist",
			path:       "./invalid",
			wantErr:    &errRecurseIgnore{Err: fs.ErrNotExist},
			wantErrStr: "lstat invalid",
		},
		{
			name: "path is not a directory",
			path: "./file.txt",
			nodes: []*kusNode{
				{
					path: "file.txt",
				},
			},
			wantErr:    &errRecurseIgnore{Err: fmt.Errorf("not a directory")},
			wantErrStr: "not a directory",
		},
		{
			name: "recurse error is returned",
			path: "/foo",
			nodes: []*kusNode{
				{
					path:          "foo/kustomization.yaml",
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "baz/wrongfile.yaml",
					expectVisited: 0,
					expectCached:  false,
				},
			},
			wantErr: fmt.Errorf("no kustomization file found"),
		},
		{
			name: "recurse ignores errRecurseIgnore",
			path: "/foo",
			nodes: []*kusNode{
				{
					path:          "foo/kustomization.yaml",
					resources:     []string{"../baz"},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "baz",
					expectVisited: 0,
					expectCached:  false,
				},
			},
		},
		{
			name: "remote build references are ignored",
			path: "/foo",
			nodes: []*kusNode{
				{
					path: "foo/kustomization.yaml",
					resources: []string{
						"../baz",
						"https://github.com/kubernetes-sigs/kustomize//examples/multibases/dev/?ref=v1.0.6",
					},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path: "baz/kustomization.yaml",
					resources: []string{
						"github.com/Liujingfang1/mysql?ref=test",
					},
					expectVisited: 1,
					expectCached:  true,
				},
			},
		},
		{
			name: "visit error is returned",
			path: "/",
			nodes: []*kusNode{
				{
					path: "kustomization.yaml",
					resources: []string{
						"baz",
					},
					expectVisited: 1,
					expectCached:  true,
				},
				{
					path:          "baz/kustomization.yaml",
					visitErr:      fmt.Errorf("visit error"),
					expectVisited: 1,
					expectCached:  true,
				},
			},
			wantErr:    fmt.Errorf("visit error"),
			wantErrStr: "visit error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			tmpDir := t.TempDir()

			for _, n := range tt.nodes {
				path := filepath.Join(tmpDir, n.path)
				if n.symlink != "" {
					g.Expect(os.Symlink(strings.Replace(n.symlink, "<tmpdir>", tmpDir, 1), path)).To(Succeed())
					return
				}
				kus := kustypes.Kustomization{
					TypeMeta: kustypes.TypeMeta{
						APIVersion: kustypes.KustomizationVersion,
						Kind:       kustypes.KustomizationKind,
					},
				}
				for _, res := range n.resources {
					res = strings.Replace(res, "<tmpdir>", tmpDir, 1)
					kus.Resources = append(kus.Resources, res)
				}
				b, err := yaml.Marshal(kus)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(os.MkdirAll(filepath.Dir(path), 0o700)).To(Succeed())
				g.Expect(os.WriteFile(path, b, 0o644))
			}

			visit := func(root, path string, kus *kustypes.Kustomization) error {
				if filepath.IsAbs(path) {
					path = stripRoot(root, path)
				}
				for _, n := range tt.nodes {
					if dir := filepath.Dir(n.path); filepath.Join(tt.wordirSuffix, path) != dir {
						continue
					}
					n.visited++
					if n.visitErr != nil {
						return n.visitErr
					}
				}
				return nil
			}

			visited := make(map[string]struct{}, 0)
			err := recurseKustomizationFiles(filepath.Join(tmpDir, tt.wordirSuffix), tt.path, visit, visited)
			if tt.wantErr != nil {
				g.Expect(err).To(HaveOccurred())
				g.Expect(err).To(BeAssignableToTypeOf(tt.wantErr))
				if tt.wantErrStr != "" {
					g.Expect(err.Error()).To(ContainSubstring(tt.wantErrStr))
				}
				return
			}

			g.Expect(err).ToNot(HaveOccurred())
			for _, n := range tt.nodes {
				g.Expect(n.visited).To(Equal(n.expectVisited), n.path)

				haveCache := HaveKey(filepath.Dir(filepath.Join(tmpDir, n.path)))
				if n.expectCached {
					g.Expect(visited).To(haveCache)
				} else {
					g.Expect(visited).ToNot(haveCache)
				}
			}
		})
	}
}

func Test_isSOPSEncryptedResource(t *testing.T) {
	g := NewWithT(t)

	resourceFactory := provider.NewDefaultDepProvider().GetResourceFactory()
	encrypted := resourceFactory.FromMap(map[string]interface{}{
		"sops": map[string]string{
			"mac": "some mac value",
		},
	})
	empty := resourceFactory.FromMap(map[string]interface{}{})

	g.Expect(isSOPSEncryptedResource(encrypted)).To(BeTrue())
	g.Expect(isSOPSEncryptedResource(empty)).To(BeFalse())
}

func Test_secureAbsPath(t *testing.T) {
	tests := []struct {
		name    string
		root    string
		path    string
		wantAbs string
		wantRel string
		wantErr bool
	}{
		{
			name:    "absolute to root",
			root:    "/wordir/",
			path:    "/wordir/foo/",
			wantAbs: "/wordir/foo",
			wantRel: "foo",
		},
		{
			name:    "relative to root",
			root:    "/wordir",
			path:    "./foo",
			wantAbs: "/wordir/foo",
			wantRel: "foo",
		},
		{
			name:    "illegal traverse",
			root:    "/wordir/foo",
			path:    "../../bar",
			wantAbs: "/wordir/foo/bar",
			wantRel: "bar",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			gotAbs, gotRel, err := securePaths(tt.root, tt.path)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				g.Expect(gotAbs).To(BeEmpty())
				g.Expect(gotRel).To(BeEmpty())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(gotAbs).To(Equal(tt.wantAbs))
			g.Expect(gotRel).To(Equal(tt.wantRel))
		})
	}
}
