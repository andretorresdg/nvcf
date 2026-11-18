/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * SPDX-License-Identifier: Apache-2.0
 */
package com.nvidia.boot.jwt.jwks;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

import java.io.IOException;
import java.net.ConnectException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpHeaders;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import javax.net.ssl.SSLHandshakeException;
import org.junit.jupiter.api.Test;

class RetryingJwksRetrieverTest {
    @Test
    @SuppressWarnings({"unchecked", "rawtypes"})
    void connectionRefusalIsRetriedAndThenRecovers() throws Exception {
        var client = mock(HttpClient.class);
        HttpResponse<byte[]> response = mock(HttpResponse.class);
        when(response.statusCode()).thenReturn(200);
        when(response.headers()).thenReturn(HttpHeaders.of(Map.of(), (a, b) -> true));
        when(response.body()).thenReturn("{\"keys\":[]}".getBytes(StandardCharsets.UTF_8));
        when(client.sendAsync(any(), any(HttpResponse.BodyHandler.class)))
                .thenReturn(CompletableFuture.failedFuture(new ConnectException("refused")))
                .thenReturn(CompletableFuture.failedFuture(new ConnectException("refused")))
                .thenReturn((CompletableFuture) CompletableFuture.completedFuture(response));
        var resource = new RetryingJwksRetriever(client)
                .retrieveResource(URI.create("http://keys.example.com/jwks").toURL());
        assertThat(resource.getContent()).isEqualTo("{\"keys\":[]}");
        verify(client, times(3)).sendAsync(any(), any(HttpResponse.BodyHandler.class));
    }

    @Test
    @SuppressWarnings({"unchecked", "rawtypes"})
    void untrustedCertificateIsNotRetried() {
        var client = mock(HttpClient.class);
        when(client.sendAsync(any(), any(HttpResponse.BodyHandler.class)))
                .thenReturn(CompletableFuture.failedFuture(
                        new SSLHandshakeException("untrusted certificate")));
        var retriever = new RetryingJwksRetriever(client);
        assertThatThrownBy(() -> retriever.retrieveResource(
                URI.create("https://keys.example.com/jwks").toURL()))
                .isInstanceOf(SSLHandshakeException.class);
        verify(client).sendAsync(any(), any(HttpResponse.BodyHandler.class));
    }

    @Test
    @SuppressWarnings({"unchecked", "rawtypes"})
    void interruptedCallerCancelsFetchAndDoesNotRetry() throws Exception {
        var client = mock(HttpClient.class);
        var pending = new CompletableFuture<HttpResponse<byte[]>>();
        when(client.sendAsync(any(), any(HttpResponse.BodyHandler.class)))
                .thenReturn((CompletableFuture) pending);
        Thread.currentThread().interrupt();
        try {
            assertThatThrownBy(() -> new RetryingJwksRetriever(client).retrieveResource(
                    URI.create("http://keys.example.com/jwks").toURL()))
                    .isInstanceOf(IOException.class);
            assertThat(Thread.currentThread().isInterrupted()).isTrue();
            assertThat(pending.isCancelled()).isTrue();
            verify(client).sendAsync(any(), any(HttpResponse.BodyHandler.class));
        } finally {
            Thread.interrupted();
        }
    }
}
