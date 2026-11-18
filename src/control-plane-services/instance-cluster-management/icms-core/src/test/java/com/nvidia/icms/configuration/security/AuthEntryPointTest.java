/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * SPDX-License-Identifier: Apache-2.0
 */
package com.nvidia.icms.configuration.security;

import static org.assertj.core.api.Assertions.assertThat;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.ArgumentMatchers.isNull;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.verify;

import com.nimbusds.jose.jwk.source.JWKSetRetrievalException;
import com.nvidia.boot.exceptions.ServiceUnavailableException;
import com.nvidia.icms.errors.IcmsAuthenticationException;
import java.io.IOException;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;
import org.springframework.http.HttpStatus;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.mock.web.MockHttpServletResponse;
import org.springframework.security.authentication.AuthenticationServiceException;
import org.springframework.security.oauth2.jwt.JwtException;
import org.springframework.security.oauth2.server.resource.InvalidBearerTokenException;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.web.servlet.HandlerExceptionResolver;

class AuthEntryPointTest {
    @Test
    void keyServiceOutageIsResolvedAs503() throws Exception {
        var resolver = mock(HandlerExceptionResolver.class);
        var entry = new AuthEntryPoint();
        ReflectionTestUtils.setField(entry, "exceptionResolver", resolver);
        var request = new MockHttpServletRequest();
        var response = new MockHttpServletResponse();
        var source = new JWKSetRetrievalException("unavailable", new IOException("refused"));
        entry.commence(request, response, new AuthenticationServiceException("key fetch failed",
                new JwtException("key fetch failed", source)));
        var exception = ArgumentCaptor.forClass(ServiceUnavailableException.class);
        verify(resolver).resolveException(eq(request), eq(response), isNull(), exception.capture());
        assertThat(exception.getValue().getStatusCode()).isEqualTo(HttpStatus.SERVICE_UNAVAILABLE);
        assertThat(response.getHeader("WWW-Authenticate")).isNull();
    }

    @Test
    void invalidBearerKeepsExisting401Handling() throws Exception {
        var resolver = mock(HandlerExceptionResolver.class);
        var entry = new AuthEntryPoint();
        ReflectionTestUtils.setField(entry, "exceptionResolver", resolver);
        var request = new MockHttpServletRequest();
        var response = new MockHttpServletResponse();
        entry.commence(request, response, new InvalidBearerTokenException("bad signature"));
        verify(resolver).resolveException(eq(request), eq(response), isNull(),
                any(IcmsAuthenticationException.class));
        assertThat(response.getStatus()).isEqualTo(401);
    }
}
